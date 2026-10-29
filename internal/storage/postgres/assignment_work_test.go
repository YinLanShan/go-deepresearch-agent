package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// seedRunTask 建好 Session → Task → RunTask 三层，返回 RunTask。
func seedRunTask(t *testing.T, db *sql.DB, idx int) *domain.RunTask {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Millisecond)

	sess := &domain.Session{ID: sessionID(idx), Title: "s", CreatedAt: base, UpdatedAt: base}
	if err := NewSessionStore(db).Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	task := &domain.Task{ID: taskID(idx), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
	if err := NewTaskStore(db).Create(ctx, task); err != nil {
		t.Fatalf("task: %v", err)
	}
	rt := &domain.RunTask{
		ID: runTaskID(idx), TaskID: task.ID,
		Status: domain.RunStatusRunning, CreatedAt: base, UpdatedAt: base,
	}
	if err := NewRunTaskStore(db).Create(ctx, rt); err != nil {
		t.Fatalf("run task: %v", err)
	}
	return rt
}

func newAssignment(id, runTaskID string, turn int) *domain.Assignment {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Assignment{
		ID:        id,
		SessionID: sessionID(0),
		TaskID:    taskID(0),
		RunTaskID: runTaskID,
		Goal:      "研究目标 " + id,
		Status:    domain.AssignmentScheduled,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// ─── AssignmentStore ────────────────────────────────────────────────────────

func TestPGAssignmentRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 0)
	store := NewAssignmentStore(db)

	in := newAssignment(assignmentID(1), rt.ID, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Goal != in.Goal {
		t.Fatalf("goal = %q, want %q", got.Goal, in.Goal)
	}
	if got.Status != domain.AssignmentScheduled {
		t.Fatalf("status = %s", got.Status)
	}
	// 未完成的 Assignment，FinishedAt 应为零值（不是 0001 之外的东西）。
	if !got.FinishedAt.IsZero() {
		t.Fatalf("finished_at should be zero, got %v", got.FinishedAt)
	}
}

func TestPGAssignmentFinishedAtRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 1)
	store := NewAssignmentStore(db)

	in := newAssignment(assignmentID(2), rt.ID, 1)
	in.Status = domain.AssignmentCompleted
	in.Result = "研究结果"
	in.FinishedAt = in.CreatedAt.Add(2 * time.Second)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Result != in.Result {
		t.Fatalf("result = %q, want %q", got.Result, in.Result)
	}
	if !got.FinishedAt.UTC().Equal(in.FinishedAt.UTC()) {
		t.Fatalf("finished_at = %v, want %v", got.FinishedAt.UTC(), in.FinishedAt.UTC())
	}
}

func TestPGAssignmentDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 2)
	store := NewAssignmentStore(db)

	in := newAssignment(assignmentID(3), rt.ID, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Create(ctx, in); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestPGAssignmentUpdateNotFound(t *testing.T) {
	db := testDB(t)
	store := NewAssignmentStore(db)

	err := store.Update(context.Background(), newAssignment(assignmentID(4), runTaskID(99), 1))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPGAssignmentListByRunTask(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rtA := seedRunTask(t, db, 10)
	rtB := seedRunTask(t, db, 11)
	store := NewAssignmentStore(db)

	// A 下两个，B 下一个。
	for i := 0; i < 2; i++ {
		a := newAssignment(assignmentID(10+i), rtA.ID, 1)
		a.CreatedAt = rtA.CreatedAt.Add(time.Duration(i) * time.Second)
		if err := store.Create(ctx, a); err != nil {
			t.Fatalf("create A%d: %v", i, err)
		}
	}
	if err := store.Create(ctx, newAssignment(assignmentID(20), rtB.ID, 1)); err != nil {
		t.Fatalf("create B: %v", err)
	}

	out, err := store.ListByRunTask(ctx, rtA.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].CreatedAt.After(out[i].CreatedAt) {
			t.Fatal("assignments should be ordered by created_at asc")
		}
	}
}

// TestPGAssignmentDeleteByRunTaskIdempotent 验证级联删除幂等（删 0 行不报错）。
func TestPGAssignmentDeleteByRunTaskIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 3)
	store := NewAssignmentStore(db)

	if err := store.DeleteByRunTask(ctx, rt.ID); err != nil {
		t.Fatalf("delete on empty should be nil, got %v", err)
	}

	in := newAssignment(assignmentID(30), rt.ID, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.DeleteByRunTask(ctx, rt.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, in.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	// 再次删除仍应成功（幂等）。
	if err := store.DeleteByRunTask(ctx, rt.ID); err != nil {
		t.Fatalf("second delete should be nil, got %v", err)
	}
}

// TestPGAssignmentCascadeOnRunTaskDelete 验证删除 RunTask 级联删除 Assignment。
func TestPGAssignmentCascadeOnRunTaskDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 4)
	store := NewAssignmentStore(db)

	in := newAssignment(assignmentID(31), rt.ID, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := NewRunTaskStore(db).Delete(ctx, rt.ID); err != nil {
		t.Fatalf("run task delete: %v", err)
	}
	if _, err := store.Get(ctx, in.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("assignment should be cascade-deleted, got %v", err)
	}
}

// ─── WorkStore ──────────────────────────────────────────────────────────────

func newWork(id, assignmentID, runTaskID string) *domain.Work {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Work{
		ID:           id,
		AssignmentID: assignmentID,
		SessionID:    sessionID(0),
		TaskID:       taskID(0),
		RunTaskID:    runTaskID,
		Goal:         "子任务 " + id,
		Status:       domain.WorkRunning,
		WorkerID:     "w-1",
		Attempt:      1,
		Stage:        "searching",
		Detail:       "检索中",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// seedAssignment 建好 Assignment 供 Work 挂载。
func seedAssignment(t *testing.T, db *sql.DB, rt *domain.RunTask, idx int) *domain.Assignment {
	t.Helper()
	a := newAssignment(assignmentID(idx), rt.ID, 1)
	if err := NewAssignmentStore(db).Create(context.Background(), a); err != nil {
		t.Fatalf("assignment: %v", err)
	}
	return a
}

func TestPGWorkRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 20)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(40), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}

	in := newWork(workID(1), a.ID, rt.ID)
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := works.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Goal != in.Goal || got.WorkerID != in.WorkerID {
		t.Fatalf("unexpected work: %+v", got)
	}
	if got.Status != domain.WorkRunning || got.Attempt != 1 {
		t.Fatalf("unexpected status/attempt: %+v", got)
	}
	// 未设置 Constraint 时应为 nil（不是空结构体）。
	if got.Constraint != nil {
		t.Fatalf("constraint should be nil, got %+v", got.Constraint)
	}
	// 未开始/未结束 → 零值时间。
	if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Fatalf("started/finished should be zero, got %v / %v", got.StartedAt, got.FinishedAt)
	}
}

func TestPGWorkConstraintRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 21)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(41), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}

	in := newWork(workID(2), a.ID, rt.ID)
	in.Constraint = &domain.WorkConstraint{
		ToolAllowed: []string{"web_search", "fetch"},
		TimeLimit:   90 * time.Second,
	}
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := works.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Constraint == nil {
		t.Fatal("constraint should be restored")
	}
	if len(got.Constraint.ToolAllowed) != 2 || got.Constraint.ToolAllowed[0] != "web_search" {
		t.Fatalf("tool_allowed = %v", got.Constraint.ToolAllowed)
	}
	if got.Constraint.TimeLimit != 90*time.Second {
		t.Fatalf("time_limit = %v", got.Constraint.TimeLimit)
	}
}

func TestPGWorkTimestampsRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 22)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(42), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}

	in := newWork(workID(3), a.ID, rt.ID)
	in.Status = domain.WorkSucceeded
	in.Result = "产物"
	in.StartedAt = in.CreatedAt.Add(time.Second)
	in.FinishedAt = in.CreatedAt.Add(3 * time.Second)
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := works.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.StartedAt.UTC().Equal(in.StartedAt.UTC()) {
		t.Fatalf("started_at = %v, want %v", got.StartedAt.UTC(), in.StartedAt.UTC())
	}
	if !got.FinishedAt.UTC().Equal(in.FinishedAt.UTC()) {
		t.Fatalf("finished_at = %v, want %v", got.FinishedAt.UTC(), in.FinishedAt.UTC())
	}
	if got.Result != in.Result {
		t.Fatalf("result = %q", got.Result)
	}
}

func TestPGWorkUpdate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 23)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(43), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}
	in := newWork(workID(4), a.ID, rt.ID)
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 模拟执行完成：改状态、补结果、写结束时间。
	in.Status = domain.WorkSucceeded
	in.Stage = "finished"
	in.Result = "完成"
	in.FinishedAt = in.CreatedAt.Add(5 * time.Second)
	in.UpdatedAt = in.FinishedAt
	if err := works.Update(ctx, in); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := works.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.WorkSucceeded || got.Stage != "finished" || got.Result != "完成" {
		t.Fatalf("update not applied: %+v", got)
	}
	if !got.FinishedAt.UTC().Equal(in.FinishedAt.UTC()) {
		t.Fatalf("finished_at = %v, want %v", got.FinishedAt.UTC(), in.FinishedAt.UTC())
	}
}

func TestPGWorkUpdateNotFound(t *testing.T) {
	db := testDB(t)
	works := NewWorkStore(db)

	err := works.Update(context.Background(), newWork(workID(5), assignmentID(99), runTaskID(99)))
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPGWorkDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 24)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(44), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}
	in := newWork(workID(6), a.ID, rt.ID)
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := works.Create(ctx, in); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestPGWorkListByRunTask(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 25)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(45), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}

	for i := 0; i < 3; i++ {
		w := newWork(workID(10+i), a.ID, rt.ID)
		w.CreatedAt = rt.CreatedAt.Add(time.Duration(i) * time.Second)
		if err := works.Create(ctx, w); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	out, err := works.ListByRunTask(ctx, rt.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].CreatedAt.After(out[i].CreatedAt) {
			t.Fatal("works should be ordered by created_at asc")
		}
	}
}

// TestPGWorkCascadeOnAssignmentDelete 验证删除 Assignment 级联删除 Work。
func TestPGWorkCascadeOnAssignmentDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 26)
	assignments := NewAssignmentStore(db)
	works := NewWorkStore(db)

	a := newAssignment(assignmentID(46), rt.ID, 1)
	if err := assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}
	in := newWork(workID(20), a.ID, rt.ID)
	if err := works.Create(ctx, in); err != nil {
		t.Fatalf("work: %v", err)
	}

	// 删 RunTask → 级联删 Assignment → 级联删 Work（两级外键）。
	if err := NewRunTaskStore(db).Delete(ctx, rt.ID); err != nil {
		t.Fatalf("run task delete: %v", err)
	}
	if _, err := works.Get(ctx, in.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("work should be cascade-deleted, got %v", err)
	}
}

func TestPGWorkDeleteByRunTaskIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	rt := seedRunTask(t, db, 27)
	works := NewWorkStore(db)

	if err := works.DeleteByRunTask(ctx, rt.ID); err != nil {
		t.Fatalf("delete on empty should be nil, got %v", err)
	}
}
