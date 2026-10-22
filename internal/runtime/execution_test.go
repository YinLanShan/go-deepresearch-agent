package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/resultagg"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/scheduler"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/workpool"
)

// executionHarness 装配带真实执行平面（Scheduler/WorkPool/ResultAggregator）的 Runtime。
type executionHarness struct {
	rt          *Runtime
	tasks       *memory.TaskStore
	runs        *memory.RunTaskStore
	states      *stateMemory.Store
	assignments *memory.AssignmentStore
	works       *memory.WorkStore
	bus         *event.Bus
}

func newExecutionRuntime(t *testing.T) *executionHarness {
	t.Helper()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	states := stateMemory.NewStore()
	assignments := memory.NewAssignmentStore()
	works := memory.NewWorkStore()
	bus := event.NewBus()

	agg := resultagg.New(resultagg.Config{States: states, Bus: bus})
	pool := workpool.New(workpool.Config{
		Works: works, Bus: bus,
		Worker: workpool.NewMockWorker(5 * time.Millisecond), Workers: 2,
	})
	sched := scheduler.New(scheduler.Config{
		Assignments: assignments, Works: works, Pool: pool, Slots: agg,
		Bus: bus, MaxConcurrency: 2,
	})
	agg.SetSchedulerNotifier(sched)

	rt := New(runs, tasks, states, mockModel.New(), bus)
	rt.SetExecution(assignments, works, sched, pool, agg)
	rt.Start()

	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = rt.Shutdown(sctx)
	})

	return &executionHarness{
		rt: rt, tasks: tasks, runs: runs, states: states,
		assignments: assignments, works: works, bus: bus,
	}
}

// TestResearchFinishedCancelsPendingAssignments 验证模型宣布研究结束时，
// 无论是否仍有 Assignment 在运行或等待提交，一律取消。
func TestResearchFinishedCancelsPendingAssignments(t *testing.T) {
	h := newExecutionRuntime(t)
	ctx := context.Background()

	if err := h.tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "t"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	enable(t, h.bus, "t1")
	waitStatus(t, h.runs, "t1", domain.RunStatusRunning)

	rt, err := h.runs.GetByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get run task: %v", err)
	}

	// 构造一个"正在运行"的 Assignment 与一个"等待提交"的 Assignment。
	running := &domain.Assignment{
		ID: "a-running", SessionID: "s1", TaskID: "t1", RunTaskID: rt.ID,
		Goal: "运行中的目标", Status: domain.AssignmentRunning,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	awaiting := &domain.Assignment{
		ID: "a-awaiting", SessionID: "s1", TaskID: "t1", RunTaskID: rt.ID,
		Goal: "等待提交的目标", Status: domain.AssignmentAwaitingResult,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	for _, a := range []*domain.Assignment{running, awaiting} {
		if err := h.assignments.Create(ctx, a); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}

	// 模型宣布研究结束。
	if err := h.bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.ResearchFinishedEvent{SessionID: "s1", TaskID: "t1", RunTaskID: rt.ID},
	}); err != nil {
		t.Fatalf("publish finished: %v", err)
	}

	waitStatus(t, h.runs, "t1", domain.RunStatusFinished)

	for _, id := range []string{"a-running", "a-awaiting"} {
		a, err := h.assignments.Get(ctx, id)
		if err != nil {
			t.Fatalf("get assignment %s: %v", id, err)
		}
		if a.Status != domain.AssignmentCancelled {
			t.Fatalf("assignment %s status = %q, want cancelled", id, a.Status)
		}
	}
}
