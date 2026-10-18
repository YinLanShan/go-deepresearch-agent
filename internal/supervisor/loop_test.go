package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
)

// TestMultiTurnClosedLoop 验证 SupervisorLoop 的最小闭环：
// 用户消息经事件进入 TurnLoop，模型（mock）产出研究结论，写回 ResearchState。
func TestMultiTurnClosedLoop(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "s1", "研究某框架的架构")); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := New(Config{
		Model:     mockModel.New(),
		States:    states,
		SessionID: "s1",
		Bus:       bus,
	})
	loop.Start()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = loop.Shutdown(sctx)
	}()

	// 第一轮：用户输入 → Supervisor 研究 → 结果写回。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{SessionID: "s1", Message: "第一轮问题"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFindings(t, states, "s1", 1)

	// 第二轮：再次输入 → 再次研究 → 追加结果。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{SessionID: "s1", Message: "第二轮问题"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFindings(t, states, "s1", 2)

	st, err := states.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Requirement != "研究某框架的架构" {
		t.Fatalf("requirement should be preserved, got %q", st.Requirement)
	}
	if len(st.Findings) != 2 {
		t.Fatalf("want 2 findings, got %d", len(st.Findings))
	}
	// mock 模型回显最后一条用户消息，验证结果确实来源于本轮输入。
	if st.Findings[0].Content != "mock reply to: 第一轮问题" {
		t.Fatalf("finding[0] = %q", st.Findings[0].Content)
	}
	if st.Findings[1].Content != "mock reply to: 第二轮问题" {
		t.Fatalf("finding[1] = %q", st.Findings[1].Content)
	}
	if st.Version != 2 {
		t.Fatalf("want version 2, got %d", st.Version)
	}
}

func waitFindings(t *testing.T, states researchstate.Store, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := states.Get(context.Background(), sessionID)
		if err == nil && len(st.Findings) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := states.Get(context.Background(), sessionID)
	t.Fatalf("want %d findings, got %+v", want, st)
}

// TestPreemptCapturesInterruptedContext 验证抢占：
// 模型慢速流式输出时，新 Commit 事件抢占当前轮次，中断前产生的上下文被保存，
// 抢占后开启重新规划轮次，最终 Finding 由"被中断上下文 + 模型回答"构成并 Commit。
func TestPreemptCapturesInterruptedContext(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "s1", "研究需求")); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := New(Config{
		Model:     mockModel.NewSlowModel(30 * time.Millisecond),
		States:    states,
		SessionID: "s1",
		Bus:       bus,
	})
	loop.Start()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = loop.Shutdown(sctx)
	}()

	// 用户消息 → 模型慢速流式输出
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{SessionID: "s1", Message: "请分析架构"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 等模型输出部分 chunk 后，模拟一次外部 Commit 抢占
	time.Sleep(80 * time.Millisecond)
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.ResearchStateCommittedEvent{SessionID: "s1", Version: 1}}); err != nil {
		t.Fatalf("publish preempt: %v", err)
	}

	// 抢占后开启重新规划轮次，最终应 Commit 一条 Finding
	waitFindings(t, states, "s1", 1)

	st, err := states.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Results) != 1 {
		t.Fatalf("want 1 committed result, got %+v", st.Results)
	}
	if !strings.Contains(st.Findings[0].Content, "mock reply to:") {
		t.Fatalf("finding should contain model answer, got %q", st.Findings[0].Content)
	}
	// 中断上下文已合并进 Finding，Commit 后应被清空
	if st.InterruptedContext != "" {
		t.Fatalf("interrupted context should be cleared after commit, got %q", st.InterruptedContext)
	}
}
