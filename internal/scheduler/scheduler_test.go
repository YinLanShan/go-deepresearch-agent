package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// fakePool 记录被提交的 Work，并可控制其何时"完成"。
type fakePool struct {
	mu                   sync.Mutex
	submitted            []*domain.Work
	cancelN              int
	cancelledAssignments []string
	failWith             error
}

func (f *fakePool) Submit(_ context.Context, w *domain.Work) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	cp := *w
	f.submitted = append(f.submitted, &cp)
	return nil
}

func (f *fakePool) Cancel(string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelN++
	return 0
}

func (f *fakePool) CancelByAssignment(_, assignmentID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelledAssignments = append(f.cancelledAssignments, assignmentID)
	return 1
}

func (f *fakePool) submittedWorks() []*domain.Work {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Work, len(f.submitted))
	copy(out, f.submitted)
	return out
}

// fakeSlots 记录槽位预留与释放。
type fakeSlots struct {
	mu            sync.Mutex
	reserved      []string
	cancelled     []string
	revoked       []string
	revokeReasons []string
	failWith      error
}

func (f *fakeSlots) Reserve(a *domain.Assignment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.reserved = append(f.reserved, a.ID)
	return nil
}

func (f *fakeSlots) Cancel(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
}

// Revoke 记录被撤销的 Assignment（门控标记 + 已提交结果撤回）。
func (f *fakeSlots) Revoke(_ string, ids []string, reason string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, ids...)
	f.revokeReasons = append(f.revokeReasons, reason)
	return 0
}

func (f *fakeSlots) snapshot() (reserved, cancelled []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reserved...), append([]string(nil), f.cancelled...)
}

func newTestScheduler(t *testing.T, pool WorkRunner, slots ResultSlots, maxConc int) (*Scheduler, *memory.AssignmentStore, *event.Bus) {
	t.Helper()
	assignments := memory.NewAssignmentStore()
	works := memory.NewWorkStore()
	bus := event.NewBus()
	s := New(Config{
		Assignments:    assignments,
		Works:          works,
		Pool:           pool,
		Slots:          slots,
		Bus:            bus,
		MaxConcurrency: maxConc,
	})
	s.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s, assignments, bus
}

func newAssignment(id, runTaskID string) *domain.Assignment {
	now := time.Now()
	return &domain.Assignment{
		ID: id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.AssignmentPending,
		CreatedAt: now, UpdatedAt: now,
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestSchedulerReservesSlotBeforeDispatch 验证"先预留槽位，再进入调度"。
func TestSchedulerReservesSlotBeforeDispatch(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	_, assignments, bus := newTestScheduler(t, pool, slots, 2)

	a := newAssignment("a1", "r1")
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Assignments: []*domain.Assignment{a},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if !waitFor(t, 2*time.Second, func() bool { return len(pool.submittedWorks()) == 1 }) {
		t.Fatalf("work not submitted, pool=%d", len(pool.submittedWorks()))
	}
	reserved, _ := slots.snapshot()
	if len(reserved) != 1 || reserved[0] != "a1" {
		t.Fatalf("slot should be reserved before dispatch, got %v", reserved)
	}

	got, err := assignments.Get(context.Background(), "a1")
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.Status != domain.AssignmentRunning {
		t.Fatalf("assignment status = %q, want running", got.Status)
	}

	w := pool.submittedWorks()[0]
	if w.AssignmentID != "a1" || w.Attempt != 1 {
		t.Fatalf("unexpected work: %+v", w)
	}
}

// TestSchedulerSlotReserveFailureCancelsAssignment 验证预留失败时：
// 不调度 Work，但把 Assignment 标记为 cancelled 并落库（不再是静默跳过）。
func TestSchedulerSlotReserveFailureCancelsAssignment(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{failWith: errors.New("slot busy")}
	_, assignments, bus := newTestScheduler(t, pool, slots, 2)

	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Assignments: []*domain.Assignment{newAssignment("a1", "r1")},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if n := len(pool.submittedWorks()); n != 0 {
		t.Fatalf("work should not be submitted when slot reservation fails, got %d", n)
	}

	// Q4：必须留下终态，避免出现"永远不调度、也没有终态"的僵尸任务。
	got, err := assignments.Get(context.Background(), "a1")
	if err != nil {
		t.Fatalf("assignment should be persisted as cancelled, got %v", err)
	}
	if got.Status != domain.AssignmentCancelled {
		t.Fatalf("assignment status = %s, want cancelled", got.Status)
	}
	if got.Err == "" {
		t.Fatal("cancelled assignment should record the failure reason")
	}
}

// TestSchedulerStopCancelsAssignments 验证全局中止只做状态标记。
func TestSchedulerStopCancelsAssignments(t *testing.T) {
	pool := &fakePool{failWith: errors.New("hold")} // 让 Work 提交失败，Assignment 保持 running 之外的形态
	slots := &fakeSlots{}
	s, assignments, bus := newTestScheduler(t, pool, slots, 1)

	a := newAssignment("a1", "r1")
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Assignments: []*domain.Assignment{a},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		got, err := assignments.Get(context.Background(), "a1")
		return err == nil && got.Status == domain.AssignmentFailed
	}) {
		t.Fatal("assignment should become failed after submit error")
	}

	// 再建一个处于 scheduled 的 Assignment，然后执行全局中止。
	b := newAssignment("a2", "r2")
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r2",
			Assignments: []*domain.Assignment{b},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, err := assignments.Get(context.Background(), "a2")
		return err == nil
	})

	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.CoordinationRequestedEvent{TaskID: "t1", RunTaskID: "r2", Action: event.ActionStop},
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got, err := assignments.Get(context.Background(), "a2")
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.Status != domain.AssignmentCancelled {
		t.Fatalf("assignment status = %q, want cancelled", got.Status)
	}
	if pool.cancelN == 0 {
		t.Fatal("scheduler should ask pool to cancel")
	}
	_ = s
}

// TestSchedulerOnResultCommitted 验证只有提交确认后才置 completed。
func TestSchedulerOnResultCommitted(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, assignments, bus := newTestScheduler(t, pool, slots, 1)

	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Assignments: []*domain.Assignment{newAssignment("a1", "r1")},
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return len(pool.submittedWorks()) == 1 })

	// Work 成功 → Assignment 进入 awaiting_result（尚未 completed）
	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkUpdatedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: "a1", WorkID: "w1", Status: domain.WorkSucceeded,
		},
	}); err != nil {
		t.Fatalf("publish work update: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		got, err := assignments.Get(context.Background(), "a1")
		return err == nil && got.Status == domain.AssignmentAwaitingResult
	}) {
		t.Fatal("assignment should be awaiting_result after work succeeded")
	}

	// 提交确认 → completed
	s.OnResultCommitted("a1", "结果内容")
	got, err := assignments.Get(context.Background(), "a1")
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.Status != domain.AssignmentCompleted || got.Result != "结果内容" {
		t.Fatalf("assignment = %+v, want completed with result", got)
	}
}
