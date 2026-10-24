package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// TestCancelAssignmentsByState 验证按 Assignment 状态分别撤销：
// 排队中的从队列移除、执行中的取消 Work、已提交的交给聚合器撤回。
func TestCancelAssignmentsByState(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, assignments, bus := newTestScheduler(t, pool, slots, 1)
	defer func() { _ = s.Shutdown(context.Background()) }()

	ctx := context.Background()
	now := time.Now()

	// 三个 Assignment：一个运行中、一个待调度、一个已完成。
	mk := func(id string, status domain.AssignmentStatus) *domain.Assignment {
		return &domain.Assignment{
			ID: id, SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Goal: "目标 " + id, Status: status, CreatedAt: now, UpdatedAt: now,
		}
	}
	running := mk("a-running", domain.AssignmentRunning)
	queued := mk("a-queued", domain.AssignmentScheduled)
	done := mk("a-done", domain.AssignmentCompleted)
	for _, a := range []*domain.Assignment{running, queued, done} {
		if err := assignments.Create(ctx, a); err != nil {
			t.Fatalf("create %s: %v", a.ID, err)
		}
		if err := slots.Reserve(a); err != nil {
			t.Fatalf("reserve %s: %v", a.ID, err)
		}
	}
	_ = bus

	// 让 "queued" 进入调度队列（通过提交事件登记）。
	s.enqueue([]*domain.Assignment{queued})

	cancelled, _, err := s.CancelAssignments("r1",
		[]string{"a-running", "a-queued", "a-done"}, "模型决定撤销")
	if err != nil {
		t.Fatalf("CancelAssignments: %v", err)
	}
	if cancelled != 3 {
		t.Fatalf("cancelled = %d, want 3", cancelled)
	}

	// 1) 状态：三个都应为 cancelled
	for _, id := range []string{"a-running", "a-queued", "a-done"} {
		a, err := assignments.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if a.Status != domain.AssignmentCancelled {
			t.Fatalf("%s status = %s, want cancelled", id, a.Status)
		}
		if a.Err != "模型决定撤销" {
			t.Fatalf("%s err = %q", id, a.Err)
		}
	}

	// 2) 队列：已清空
	if q, _ := s.Status(); q != 0 {
		t.Fatalf("queue should be cleared, got %d", q)
	}

	// 3) 执行中的 Work 被取消
	pool.mu.Lock()
	cancelledWorks := append([]string(nil), pool.cancelledAssignments...)
	pool.mu.Unlock()
	found := false
	for _, id := range cancelledWorks {
		if id == "a-running" {
			found = true
		}
	}
	if !found {
		t.Fatalf("running work should be cancelled, got %v", cancelledWorks)
	}

	// 4) 结果聚合器收到撤销标记（全部三个）
	slots.mu.Lock()
	revoked := append([]string(nil), slots.revoked...)
	slots.mu.Unlock()
	if len(revoked) != 3 {
		t.Fatalf("slots revoked = %v, want 3 entries", revoked)
	}
}

// TestCancelAssignmentsRejectsForeignRunTask 验证不能撤销其他 RunTask 的 Assignment。
func TestCancelAssignmentsRejectsForeignRunTask(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, assignments, _ := newTestScheduler(t, pool, slots, 1)
	defer func() { _ = s.Shutdown(context.Background()) }()

	ctx := context.Background()
	other := &domain.Assignment{
		ID: "a-other", SessionID: "s9", TaskID: "t9", RunTaskID: "r-other",
		Goal: "别人的目标", Status: domain.AssignmentRunning,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := assignments.Create(ctx, other); err != nil {
		t.Fatalf("create: %v", err)
	}

	cancelled, _, err := s.CancelAssignments("r1", []string{"a-other"}, "越权")
	if err != nil {
		t.Fatalf("CancelAssignments: %v", err)
	}
	if cancelled != 0 {
		t.Fatalf("foreign assignment should not be cancelled, got %d", cancelled)
	}

	a, _ := assignments.Get(ctx, "a-other")
	if a.Status != domain.AssignmentRunning {
		t.Fatalf("foreign assignment status changed to %s", a.Status)
	}
}

// TestCancelAssignmentsIdempotent 验证重复撤销是幂等的。
func TestCancelAssignmentsIdempotent(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, assignments, _ := newTestScheduler(t, pool, slots, 1)
	defer func() { _ = s.Shutdown(context.Background()) }()

	ctx := context.Background()
	a := &domain.Assignment{
		ID: "a1", SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
		Goal: "目标", Status: domain.AssignmentAwaitingResult,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := slots.Reserve(a); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if n, _, err := s.CancelAssignments("r1", []string{"a1"}, "第一次"); err != nil || n != 1 {
		t.Fatalf("first cancel: n=%d err=%v", n, err)
	}
	if n, _, err := s.CancelAssignments("r1", []string{"a1"}, "第二次"); err != nil || n != 0 {
		t.Fatalf("second cancel should be no-op: n=%d err=%v", n, err)
	}
}
