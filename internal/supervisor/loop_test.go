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

// newLoop 构造一个绑定到固定 Task/RunTask 的 SupervisorLoop。
func newLoop(t *testing.T, states researchstate.Store, bus *event.Bus, slow bool) *Loop {
	t.Helper()
	cfg := Config{
		States:    states,
		SessionID: "s1",
		TaskID:    "t1",
		RunTaskID: "r1",
		Bus:       bus,
	}
	if slow {
		cfg.Model = mockModel.NewSlowModel(30 * time.Millisecond)
	} else {
		cfg.Model = mockModel.New()
	}
	loop := New(cfg)
	loop.Start()
	return loop
}

func shutdownLoop(t *testing.T, loop *Loop) {
	t.Helper()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = loop.Shutdown(sctx)
}

// TestMultiTurnClosedLoop 验证 SupervisorLoop 的最小闭环：
// 用户消息经事件进入 TurnLoop，模型（mock）产出研究结论，写回该 Task 的 ResearchState。
func TestMultiTurnClosedLoop(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	// 第一轮：用户输入 → Supervisor 研究 → 结果写回。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "第一轮问题"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFindings(t, states, "t1", 1)

	// 第二轮：再次输入 → 再次研究 → 追加结果。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "第二轮问题"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFindings(t, states, "t1", 2)

	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	// Requirements 复数语义：两轮用户消息都按顺序保留。
	if len(st.Requirements) != 2 ||
		st.Requirements[0] != "第一轮问题" || st.Requirements[1] != "第二轮问题" {
		t.Fatalf("requirements should accumulate, got %+v", st.Requirements)
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

// TestOtherTaskMessagesIgnored 验证 SupervisorLoop 只处理本 RunTask 的消息与 Commit 事件。
func TestOtherTaskMessagesIgnored(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	// 其它 RunTask 的消息应被忽略
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t-other", RunTaskID: "r-other", Message: "别人的消息"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	st, _ := states.Get(ctx, "t1")
	if len(st.Findings) != 0 {
		t.Fatalf("other task message should be ignored, got %+v", st.Findings)
	}
}

func waitFindings(t *testing.T, states researchstate.Store, taskID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := states.Get(context.Background(), taskID)
		if err == nil && len(st.Findings) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := states.Get(context.Background(), taskID)
	t.Fatalf("want %d findings, got %+v", want, st)
}

// TestPreemptCapturesInterruptedContext 验证抢占：
// 模型慢速流式输出时，新 Commit 事件抢占当前轮次，中断前产生的上下文被保存，
// 抢占后开启重新规划轮次，最终 Commit 一条 Finding。
//
// 注意：InterruptedContext 是中间产物——被打断的原文会并入下一轮的 Finding，
// 随后被清空，不再作为独立分区进入上下文。
func TestPreemptCapturesInterruptedContext(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, true)
	defer shutdownLoop(t, loop)

	// 用户消息 → 模型慢速流式输出
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "请分析架构"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 等模型输出部分 chunk 后，模拟一次外部 Commit 抢占
	time.Sleep(80 * time.Millisecond)
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.ResearchStateCommittedEvent{
		SessionID: "s1", TaskID: "t1", Version: 1}}); err != nil {
		t.Fatalf("publish preempt: %v", err)
	}

	// 抢占后开启重新规划轮次，最终应 Commit 一条 Finding
	waitFindings(t, states, "t1", 1)

	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Results) != 1 {
		t.Fatalf("want 1 committed result, got %+v", st.Results)
	}
	if !strings.Contains(st.Findings[0].Content, "mock reply to:") {
		t.Fatalf("finding should contain model answer, got %q", st.Findings[0].Content)
	}
	// 中断原文应被并入本轮的 Finding（"重写被打断的 Finding"），
	// 随后 InterruptedContext 作为中间产物被清空，且不再作为独立分区进入上下文。
	merged := st.Findings[0].Content
	if !strings.Contains(merged, "mock reply to:") {
		t.Fatalf("finding should contain model answer, got %q", merged)
	}
	// 被打断那轮的输出（"mock reply to: 请分析架构"）也应出现在合并结果里。
	if !strings.Contains(merged, "请分析架构") {
		t.Fatalf("finding should merge interrupted output, got %q", merged)
	}
	if st.InterruptedContext != "" {
		t.Fatalf("interrupted context should be cleared after commit, got %q", st.InterruptedContext)
	}
	if strings.Contains(st.BuildContext(), "被中断的上下文原文") {
		t.Fatal("interrupted context must not be a standalone section in built context")
	}
}
