package memory

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

func newAssignment(id, runTaskID string) *domain.Assignment {
	now := time.Now()
	return &domain.Assignment{
		ID: id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.AssignmentPending,
		CreatedAt: now, UpdatedAt: now,
	}
}

func newWork(id, runTaskID string) *domain.Work {
	now := time.Now()
	return &domain.Work{
		ID: id, AssignmentID: "a-" + id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.WorkPending, Attempt: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

// TestAssignmentCascadeDelete 验证 Assignment 归属 RunTask：
// 删除某个 RunTask 时，只删除其下的 Assignment，不影响其他 RunTask。
func TestAssignmentCascadeDelete(t *testing.T) {
	ctx := context.Background()
	s := NewAssignmentStore()

	for _, a := range []*domain.Assignment{
		newAssignment("a1", "r1"), newAssignment("a2", "r1"), newAssignment("a3", "r2"),
	} {
		if err := s.Create(ctx, a); err != nil {
			t.Fatalf("create %s: %v", a.ID, err)
		}
	}

	if err := s.DeleteByRunTask(ctx, "r1"); err != nil {
		t.Fatalf("delete by run task: %v", err)
	}

	for _, id := range []string{"a1", "a2"} {
		if _, err := s.Get(ctx, id); err == nil {
			t.Fatalf("assignment %s should be deleted", id)
		}
	}
	if _, err := s.Get(ctx, "a3"); err != nil {
		t.Fatalf("assignment of another run task should remain: %v", err)
	}

	rest, err := s.ListByRunTask(ctx, "r1")
	if err != nil {
		t.Fatalf("list r1: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("r1 should have no assignments, got %d", len(rest))
	}
}

// TestWorkCascadeDelete 验证 Work 归属 RunTask 的级联删除。
func TestWorkCascadeDelete(t *testing.T) {
	ctx := context.Background()
	s := NewWorkStore()

	for _, w := range []*domain.Work{
		newWork("w1", "r1"), newWork("w2", "r1"), newWork("w3", "r2"),
	} {
		if err := s.Create(ctx, w); err != nil {
			t.Fatalf("create %s: %v", w.ID, err)
		}
	}

	if err := s.DeleteByRunTask(ctx, "r1"); err != nil {
		t.Fatalf("delete by run task: %v", err)
	}

	for _, id := range []string{"w1", "w2"} {
		if _, err := s.Get(ctx, id); err == nil {
			t.Fatalf("work %s should be deleted", id)
		}
	}
	if _, err := s.Get(ctx, "w3"); err != nil {
		t.Fatalf("work of another run task should remain: %v", err)
	}
}

// TestStoreInterfaces 确保内存实现满足存储接口（含级联删除能力）。
func TestStoreInterfaces(t *testing.T) {
	var _ storage.AssignmentStore = NewAssignmentStore()
	var _ storage.WorkStore = NewWorkStore()
}
