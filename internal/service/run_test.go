package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// recordingSender 记录收到的事件并同步回执，用于验证 RunService 发布的事件。
type recordingSender struct {
	mu  sync.Mutex
	got []event.Event
}

func (r *recordingSender) record(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
}

func (r *recordingSender) Send(_ context.Context, e event.EventEnvelope) error {
	r.record(e.Data)
	return nil
}

func (r *recordingSender) SendSync(_ context.Context, e event.EventEnvelope, h *event.Handshake) error {
	r.record(e.Data)
	h.Reply(nil)
	return nil
}

func (r *recordingSender) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.got))
	for i, ev := range r.got {
		out[i] = ev.EventType()
	}
	return out
}

func newTestRunService() (*RunService, *memory.TaskStore, *memory.RunTaskStore, *recordingSender) {
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	bus := event.NewBus()
	rec := &recordingSender{}
	bus.Subscribe(event.EventTaskEnabled, rec)
	bus.Subscribe(event.EventTaskDisabled, rec)
	return NewRunService(tasks, runs, bus), tasks, runs, rec
}

// setupTask 创建一个 Task 及关联 RunTask，作为启用/禁用的前置条件。
func setupTask(t *testing.T, tasks *memory.TaskStore, runs *memory.RunTaskStore, taskID string, enabled bool) {
	t.Helper()
	if err := tasks.Create(context.Background(), &domain.Task{ID: taskID, SessionID: "s1", Enabled: enabled}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := runs.Create(context.Background(), &domain.RunTask{ID: "r-" + taskID, TaskID: taskID, Status: domain.RunStatusCreated}); err != nil {
		t.Fatalf("create run task: %v", err)
	}
}

func TestSetTaskEnabledPublishesEnabledEvent(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rec := newTestRunService()
	setupTask(t, tasks, runs, "t1", false)

	task, err := svc.SetTaskEnabled(ctx, "t1", true)
	if err != nil {
		t.Fatalf("SetTaskEnabled: %v", err)
	}
	if !task.Enabled {
		t.Fatal("task should be enabled")
	}
	if types := rec.types(); len(types) != 1 || types[0] != event.EventTaskEnabled {
		t.Fatalf("want [%s], got %v", event.EventTaskEnabled, types)
	}
}

func TestSetTaskEnabledPublishesDisabledEvent(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rec := newTestRunService()
	setupTask(t, tasks, runs, "t1", true)

	if _, err := svc.SetTaskEnabled(ctx, "t1", false); err != nil {
		t.Fatalf("SetTaskEnabled: %v", err)
	}
	if types := rec.types(); len(types) != 1 || types[0] != event.EventTaskDisabled {
		t.Fatalf("want [%s], got %v", event.EventTaskDisabled, types)
	}
}

func TestSetTaskEnabledIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rec := newTestRunService()
	setupTask(t, tasks, runs, "t1", true)

	if _, err := svc.SetTaskEnabled(ctx, "t1", true); err != nil {
		t.Fatalf("SetTaskEnabled: %v", err)
	}
	if types := rec.types(); len(types) != 0 {
		t.Fatalf("idempotent call should publish no event, got %v", types)
	}
}

func TestSetTaskEnabledTaskNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestRunService()

	if _, err := svc.SetTaskEnabled(ctx, "missing", true); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestSetTaskEnabledWithoutRunTaskPublishes 无 RunTask 时不再预检，仍发布事件，
// 由 Runtime 负责协商创建 RunTask。
func TestSetTaskEnabledWithoutRunTaskPublishes(t *testing.T) {
	ctx := context.Background()
	svc, tasks, _, rec := newTestRunService()
	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Enabled: false}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if _, err := svc.SetTaskEnabled(ctx, "t1", true); err != nil {
		t.Fatalf("SetTaskEnabled: %v", err)
	}
	if types := rec.types(); len(types) != 1 || types[0] != event.EventTaskEnabled {
		t.Fatalf("want [%s], got %v", event.EventTaskEnabled, types)
	}
}

func TestGetRunTaskByTask(t *testing.T) {
	ctx := context.Background()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	svc := NewRunService(tasks, runs, event.NewBus())

	if err := runs.Create(ctx, &domain.RunTask{ID: "r1", TaskID: "t1", Status: domain.RunStatusRunning}); err != nil {
		t.Fatalf("create run task: %v", err)
	}

	got, err := svc.GetRunTaskByTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetRunTaskByTask: %v", err)
	}
	if got.ID != "r1" {
		t.Fatalf("unexpected run task: %+v", got)
	}
	if _, err := svc.GetRunTaskByTask(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
