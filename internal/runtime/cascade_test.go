package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// TestRunTaskDeleteCascadesAssignments 验证 Assignment 归属 RunTask：
// RunTask 被删除时，其下的 Assignment 与 Work 一并删除，其他 RunTask 不受影响。
func TestRunTaskDeleteCascadesAssignments(t *testing.T) {
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

	now := time.Now()
	// 本 RunTask 下：一个 Assignment 与其 Work
	ownAssignment := &domain.Assignment{
		ID: "a-own", SessionID: "s1", TaskID: "t1", RunTaskID: rt.ID,
		Goal: "本次运行的目标", Status: domain.AssignmentRunning, CreatedAt: now, UpdatedAt: now,
	}
	ownWork := &domain.Work{
		ID: "w-own", AssignmentID: "a-own", SessionID: "s1", TaskID: "t1", RunTaskID: rt.ID,
		Goal: "本次运行的执行", Status: domain.WorkRunning, Attempt: 1, CreatedAt: now, UpdatedAt: now,
	}
	// 另一个 RunTask 的数据，应该被保留
	otherAssignment := &domain.Assignment{
		ID: "a-other", SessionID: "s9", TaskID: "t9", RunTaskID: "r-other",
		Goal: "其他运行的目标", Status: domain.AssignmentRunning, CreatedAt: now, UpdatedAt: now,
	}
	otherWork := &domain.Work{
		ID: "w-other", AssignmentID: "a-other", SessionID: "s9", TaskID: "t9", RunTaskID: "r-other",
		Goal: "其他运行的执行", Status: domain.WorkRunning, Attempt: 1, CreatedAt: now, UpdatedAt: now,
	}

	for _, a := range []*domain.Assignment{ownAssignment, otherAssignment} {
		if err := h.assignments.Create(ctx, a); err != nil {
			t.Fatalf("create assignment %s: %v", a.ID, err)
		}
	}
	for _, w := range []*domain.Work{ownWork, otherWork} {
		if err := h.works.Create(ctx, w); err != nil {
			t.Fatalf("create work %s: %v", w.ID, err)
		}
	}

	// 请求删除 RunTask
	if err := h.bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.RunTaskDeleteRequestedEvent{TaskID: "t1"},
	}); err != nil {
		t.Fatalf("publish delete: %v", err)
	}

	// 本 RunTask 的 Assignment / Work 应被删除
	if _, err := h.assignments.Get(ctx, "a-own"); err == nil {
		t.Fatal("assignment of deleted run task should be removed")
	}
	if _, err := h.works.Get(ctx, "w-own"); err == nil {
		t.Fatal("work of deleted run task should be removed")
	}

	// 其他 RunTask 的数据应保留
	if _, err := h.assignments.Get(ctx, "a-other"); err != nil {
		t.Fatalf("assignment of another run task should remain: %v", err)
	}
	if _, err := h.works.Get(ctx, "w-other"); err != nil {
		t.Fatalf("work of another run task should remain: %v", err)
	}

	// RunTask 本身也应被删除
	if _, err := h.runs.GetByTask(ctx, "t1"); err == nil {
		t.Fatal("run task should be deleted")
	}
}

// TestProposeAssignmentsCreatesFromGoals 验证 Runtime 按模型给出的目标创建 Assignment。
func TestProposeAssignmentsCreatesFromGoals(t *testing.T) {
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

	goals := []string{"调研 A 的架构", "调研 B 的性能"}
	accepted, err := h.rt.ProposeAssignments(ctx, "t1", rt.ID, goals)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if accepted != len(goals) {
		t.Fatalf("accepted = %d, want %d", accepted, len(goals))
	}

	list, err := h.assignments.ListByRunTask(ctx, rt.ID)
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(list) != len(goals) {
		t.Fatalf("assignments = %d, want %d", len(list), len(goals))
	}
	got := map[string]bool{}
	for _, a := range list {
		got[a.Goal] = true
		if a.RunTaskID != rt.ID {
			t.Fatalf("assignment should belong to run task %s, got %s", rt.ID, a.RunTaskID)
		}
	}
	for _, g := range goals {
		if !got[g] {
			t.Fatalf("goal %q not created, got %v", g, list)
		}
	}
}
