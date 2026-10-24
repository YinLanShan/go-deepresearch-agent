package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// newRuntime 装配一个带 Mock 依赖组件与 Mock 模型的 Runtime。
func newRuntime(t *testing.T) (*Runtime, *memory.TaskStore, *memory.RunTaskStore, *stateMemory.Store, *event.Bus) {
	t.Helper()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	states := stateMemory.NewStore()
	bus := event.NewBus()
	RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")

	rt := New(runs, tasks, states, mockModel.New(), bus)
	rt.Start()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Shutdown(sctx)
	})
	return rt, tasks, runs, states, bus
}

func enable(t *testing.T, bus *event.Bus, taskID string) {
	t.Helper()
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.TaskEnabledEvent{TaskID: taskID}}); err != nil {
		t.Fatalf("enable: %v", err)
	}
}

func disable(t *testing.T, bus *event.Bus, taskID string) {
	t.Helper()
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.TaskDisabledEvent{TaskID: taskID}}); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

// TestStopFlow 验证用户主动中止的终态语义：
//   - Running -> Stopped；
//   - 中止后属于本 Task 的迟到 Result 仍可正常提交（提交门控不关闭）；
//   - Stopped 为终止态，"继续"不会复活（用户再发消息会新建 Task）。
func TestStopFlow(t *testing.T) {
	_, tasks, runs, states, bus := newRuntime(t)
	ctx := context.Background()

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "t", Description: "需求"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	enable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	// 用户主动中止 → Stopped
	if err := bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.StopRequestedEvent{SessionID: "s1", TaskID: "t1"}}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStatus(t, runs, "t1", domain.RunStatusStopped)

	// 中止后，属于该 Task 的迟到 Result 仍可提交（不被门控拒绝）
	if err := states.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "迟到结果"}}, FindingContent: "迟到结果"}); err != nil {
		t.Fatalf("late result commit after stop should be accepted, got %v", err)
	}
	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Results) != 1 {
		t.Fatalf("late result should be appended, got %+v", st.Results)
	}

	// Stopped 为终止态：再次使能不应复活为 Running
	enable(t, bus, "t1")
	time.Sleep(100 * time.Millisecond)
	rt, err := runs.GetByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get run task: %v", err)
	}
	if rt.Status != domain.RunStatusStopped {
		t.Fatalf("stopped task must not be resurrected, got %q", rt.Status)
	}
}

// TestDisableFlow 验证失能 = 用户中止：RunTask 置 Stopped（终态），
// 再次使能不能复活（用户继续研究会在新 Task 上进行）。
//
// 注意：不存在"暂停后继续"的语义——用户只有"中止"这一个选择。
func TestDisableFlow(t *testing.T) {
	_, tasks, runs, states, bus := newRuntime(t)
	ctx := context.Background()

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	enable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	disable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusStopped)

	// 失能后 Task 本身也应被置为不可用
	task, err := tasks.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Enabled {
		t.Fatalf("task should be disabled after disable")
	}

	// 中止为终态：再次使能不会复活为 Running。
	enable(t, bus, "t1")
	time.Sleep(100 * time.Millisecond)
	rt, err := runs.GetByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get run task: %v", err)
	}
	if rt.Status != domain.RunStatusStopped {
		t.Fatalf("stopped task must not be resurrected, got %q", rt.Status)
	}
	_ = states
}

// TestGlobalSingleRunning 验证全局单例运行：使能新 Session 时，
// 原来运行中的 Session 被置为 Waiting（被挤掉），而不是 Paused（用户暂停）。
func TestGlobalSingleRunning(t *testing.T) {
	_, tasks, runs, _, bus := newRuntime(t)
	ctx := context.Background()

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "a"}); err != nil {
		t.Fatalf("create t1: %v", err)
	}
	if err := tasks.Create(ctx, &domain.Task{ID: "t2", SessionID: "s2", Title: "b"}); err != nil {
		t.Fatalf("create t2: %v", err)
	}

	enable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	// 使能另一个 Session 的任务：t1 被挤掉应为 Waiting，t2 变为 Running
	enable(t, bus, "t2")
	waitStatus(t, runs, "t2", domain.RunStatusRunning)
	waitStatus(t, runs, "t1", domain.RunStatusWaiting)

	task1, _ := tasks.Get(ctx, "t1")
	if task1.Enabled {
		t.Fatalf("t1 should be disabled when another session starts running")
	}

	// 从 Waiting 点"继续"应能切回运行，并把 t2 挤成 Waiting
	enable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusRunning)
	waitStatus(t, runs, "t2", domain.RunStatusWaiting)
}

// TestResearchFinished 验证模型完成研究后 Running -> Finished，且 Task 失能。
func TestResearchFinished(t *testing.T) {
	_, tasks, runs, _, bus := newRuntime(t)
	ctx := context.Background()

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	enable(t, bus, "t1")
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	if err := bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.ResearchFinishedEvent{SessionID: "s1", TaskID: "t1"}}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	waitStatus(t, runs, "t1", domain.RunStatusFinished)

	task, _ := tasks.Get(ctx, "t1")
	if task.Enabled {
		t.Fatalf("task should be disabled after research finished")
	}
}

func waitStatus(t *testing.T, runs *memory.RunTaskStore, taskID string, want domain.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := runs.GetByTask(context.Background(), taskID)
		if err == nil && rt.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt, _ := runs.GetByTask(context.Background(), taskID)
	t.Fatalf("want status %q, got %+v", want, rt)
}
