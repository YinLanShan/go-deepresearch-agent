package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
)

// TestUserMessagePreemptsRunningTurn 验证用户消息会抢占正在运行的轮次。
//
// 抢占时被打断的输出会累积进 InterruptedContext，下一轮并入 Finding
// ——这正是"结果延迟到下一轮提交"的表现。
func TestUserMessagePreemptsRunningTurn(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	// slow 模型：让第一轮持续足够久，便于中途插入用户消息。
	loop := newLoop(t, states, bus, true)
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "第一轮问题"}}); err != nil {
		t.Fatalf("publish first: %v", err)
	}

	// 等第一轮开始输出后，插入第二条用户消息（应抢占）。
	time.Sleep(80 * time.Millisecond)
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "第二轮问题"}}); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	waitFindings(t, states, "t1", 1)

	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}

	// 两条用户消息都应累积为需求。
	if len(st.Requirements) != 2 {
		t.Fatalf("both messages should accumulate as requirements, got %+v", st.Requirements)
	}

	// 抢占后被打断的内容应并入 Finding（延迟提交）。
	var joined strings.Builder
	for _, f := range st.Findings {
		joined.WriteString(f.Content)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "第一轮问题") {
		t.Fatalf("finding should contain preempted turn output, got %q", joined.String())
	}
}

// TestUserMessageWakesIdleLoop 验证空闲时用户消息正常唤醒（不依赖抢占）。
func TestUserMessageWakesIdleLoop(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "请分析架构"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFindings(t, states, "t1", 1)
	st, _ := states.Get(ctx, "t1")
	if len(st.Requirements) != 1 || st.Requirements[0] != "请分析架构" {
		t.Fatalf("requirements = %+v", st.Requirements)
	}
}
