package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
)

// TestWakeOnAllSettledWhenIdle 验证：轮次空闲时，子任务全部终结会唤醒 Supervisor。
//
// 这是此前的缺口——子任务全部失败/被撤销时没有结果提交，
// 也就没有 ResearchStateCommitted，Supervisor 会永久空闲。
func TestWakeOnAllSettledWhenIdle(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	// 未运行任何轮次（idle）：发布"全部终结"应触发一次推理并提交 Finding。
	if err := bus.Publish(ctx, event.EventEnvelope{
		Data: event.AllAssignmentsSettledEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Total: 2, Succeeded: 0, Failed: 1, Cancelled: 1,
		},
	}); err != nil {
		t.Fatalf("publish settled: %v", err)
	}

	waitFindings(t, states, "t1", 1)
}

// TestSettledWakesOnlyOncePerIdlePeriod 验证同一次空闲期只被唤醒一次。
func TestSettledWakesOnlyOncePerIdlePeriod(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	// 连续发布两次"全部终结"：只应产生一轮 Finding。
	for i := 0; i < 2; i++ {
		if err := bus.Publish(ctx, event.EventEnvelope{
			Data: event.AllAssignmentsSettledEvent{
				SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
				Total: 1, Failed: 1,
			},
		}); err != nil {
			t.Fatalf("publish settled #%d: %v", i, err)
		}
	}

	waitFindings(t, states, "t1", 1)
	// 再等一会，确认没有第二轮被触发。
	time.Sleep(300 * time.Millisecond)
	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Findings) != 1 {
		t.Fatalf("idle period should wake only once, got %d findings", len(st.Findings))
	}
}

// TestSettledIgnoredWhileTurnActive 验证轮次运行中时"全部终结"不额外入队。
//
// 运行中的唤醒交给原有的抢占机制（结果提交抢占），避免同一时刻两个输入争抢。
func TestSettledIgnoredWhileTurnActive(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, true) // slow：让轮次持续一段时间
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "开始研究"}}); err != nil {
		t.Fatalf("publish user message: %v", err)
	}

	// 等轮次真正开始（turnActive=true）。
	time.Sleep(20 * time.Millisecond)
	if err := bus.Publish(ctx, event.EventEnvelope{
		Data: event.AllAssignmentsSettledEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Total: 1, Succeeded: 1,
		},
	}); err != nil {
		t.Fatalf("publish settled: %v", err)
	}

	// 只应有一轮（settled 事件不入队）。
	waitFindings(t, states, "t1", 1)
	time.Sleep(300 * time.Millisecond)
	st, _ := states.Get(ctx, "t1")
	if len(st.Findings) != 1 {
		t.Fatalf("settled event must not enqueue while turn active, got %d findings", len(st.Findings))
	}
}

// TestSettledIgnoredAfterFinish 验证进入终结流程后不再被唤醒。
func TestSettledIgnoredAfterFinish(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	loop.RequestFinish("最终结论")

	if err := bus.Publish(ctx, event.EventEnvelope{
		Data: event.AllAssignmentsSettledEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Total: 1, Succeeded: 1,
		},
	}); err != nil {
		t.Fatalf("publish settled: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	st, _ := states.Get(ctx, "t1")
	if len(st.Findings) != 0 {
		t.Fatalf("settled event must be ignored after finish, got %d findings", len(st.Findings))
	}
}

// TestSettledIgnoresOtherRunTask 验证只响应本 RunTask 的事件。
func TestSettledIgnoresOtherRunTask(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	loop := newLoop(t, states, bus, false)
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{
		Data: event.AllAssignmentsSettledEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "other-run",
			Total: 1, Failed: 1,
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	st, _ := states.Get(ctx, "t1")
	if len(st.Findings) != 0 {
		t.Fatalf("event of another run task must be ignored, got %d findings", len(st.Findings))
	}
}
