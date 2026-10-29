package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// seedTask 建好 Session → Task，返回 Task（ResearchState 归属 Task）。
func seedTask(t *testing.T, db *sql.DB, idx int) *domain.Task {
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
	return task
}

// newState 构造一份研究状态（ID 与 Task 一一对应）。
func newState(task *domain.Task, ordinal int) *researchstate.State {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &researchstate.State{
		ID:        stateIDFromTask(task.ID),
		TaskID:    task.ID,
		SessionID: task.SessionID,
		Ordinal:   ordinal,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// stateIDFromTask 从 taskID 派生 stateID（保证 32 位 hex 且每 Task 唯一）。
func stateIDFromTask(taskIDStr string) string {
	b := []byte(taskIDStr)
	b[0] = '6' // 与其他实体前缀区分
	return string(b)
}

// ─── 基础 CRUD ──────────────────────────────────────────────────────────────

func TestPGStateRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 60)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	in.Requirements = []string{"研究 A", "研究 B"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != in.ID || got.SessionID != in.SessionID || got.Ordinal != 1 {
		t.Fatalf("unexpected state: %+v", got)
	}
	if len(got.Requirements) != 2 || got.Requirements[0] != "研究 A" {
		t.Fatalf("requirements = %v", got.Requirements)
	}
	// 阶段 3：四个集合应留空。
	if len(got.Results) != 0 || len(got.Findings) != 0 ||
		len(got.CommitLog) != 0 || len(got.History) != 0 {
		t.Fatalf("stage 3 should leave collections empty: %+v", got)
	}
}

func TestPGStateCreateInvalidArgument(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	if err := store.Create(context.Background(), nil); !errors.Is(err, researchstate.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if err := store.Create(context.Background(), &researchstate.State{}); !errors.Is(err, researchstate.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for empty task id, got %v", err)
	}
}

func TestPGStateCreateDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 61)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	dup := newState(task, 2)
	dup.ID = "66666666666666666666666666666666" // 换 ID，确保冲突来自 task_id 唯一约束
	if err := store.Create(ctx, dup); !errors.Is(err, researchstate.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestPGStateNotFound(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	if _, err := store.Get(context.Background(), "nope"); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ─── requirements / related_nodes ───────────────────────────────────────────

func TestPGStateRequirementsRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 62)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 需求是"按时间顺序累积"的复数语义，**不去重**；空白内容被忽略。
	for _, r := range []string{"需求一", "需求二", "需求一", "   "} {
		if err := store.AddRequirement(ctx, task.ID, r); err != nil {
			t.Fatalf("add %q: %v", r, err)
		}
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Requirements) != 3 {
		t.Fatalf("requirements = %v, want 3 (append semantics, blank ignored)", got.Requirements)
	}
	want := []string{"需求一", "需求二", "需求一"}
	for i, w := range want {
		if got.Requirements[i] != w {
			t.Fatalf("requirements[%d] = %q, want %q", i, got.Requirements[i], w)
		}
	}
}

func TestPGStateRelatedNodesRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 63)
	store := NewResearchStateStore(db)

	in := newState(task, 2)
	in.RelatedNodes = []researchstate.RelatedNode{
		{
			TaskID:             "prev-1",
			Ordinal:            1,
			Requirements:       []string{"旧需求"},
			Results:            []researchstate.Result{{AssignmentID: "a1", Turn: 1, Content: "证据"}},
			Findings:           []researchstate.Finding{{Turn: 1, Content: "过程"}},
			Conclusion:         "旧结论",
			InterruptedContext: "未完成的原文",
		},
	}
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.RelatedNodes) != 1 {
		t.Fatalf("related nodes = %d, want 1", len(got.RelatedNodes))
	}
	n := got.RelatedNodes[0]
	if n.TaskID != "prev-1" || n.Conclusion != "旧结论" {
		t.Fatalf("unexpected node: %+v", n)
	}
	if len(n.Results) != 1 || n.Results[0].Content != "证据" {
		t.Fatalf("nested results lost: %+v", n.Results)
	}
	if n.InterruptedContext != "未完成的原文" {
		t.Fatalf("interrupted context lost: %q", n.InterruptedContext)
	}
}

// ─── 列表与最新 ─────────────────────────────────────────────────────────────

func TestPGStateListBySessionOrdered(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := NewResearchStateStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	sess := &domain.Session{ID: sessionID(70), CreatedAt: base, UpdatedAt: base}
	if err := NewSessionStore(db).Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}

	// 三个 Task，对应三个序号。
	for i := 1; i <= 3; i++ {
		task := &domain.Task{ID: taskID(70 + i), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
		if err := NewTaskStore(db).Create(ctx, task); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
		st := &researchstate.State{
			ID: stateIDFromTask(task.ID), TaskID: task.ID, SessionID: sess.ID,
			Ordinal: i, CreatedAt: base, UpdatedAt: base,
		}
		if err := store.Create(ctx, st); err != nil {
			t.Fatalf("state %d: %v", i, err)
		}
	}

	out, err := store.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].Ordinal > out[i].Ordinal {
			t.Fatalf("should be ordered by ordinal asc: %d, %d", out[i-1].Ordinal, out[i].Ordinal)
		}
	}
}

func TestPGStateGetLatestBySession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := NewResearchStateStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	sess := &domain.Session{ID: sessionID(71), CreatedAt: base, UpdatedAt: base}
	if err := NewSessionStore(db).Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	for i := 1; i <= 3; i++ {
		task := &domain.Task{ID: taskID(71 + i), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
		if err := NewTaskStore(db).Create(ctx, task); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
		st := &researchstate.State{
			ID: stateIDFromTask(task.ID), TaskID: task.ID, SessionID: sess.ID,
			Ordinal: i, CreatedAt: base, UpdatedAt: base,
		}
		if err := store.Create(ctx, st); err != nil {
			t.Fatalf("state %d: %v", i, err)
		}
	}

	got, err := store.GetLatestBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got.Ordinal != 3 {
		t.Fatalf("ordinal = %d, want 3", got.Ordinal)
	}
}

func TestPGStateGetLatestBySessionNotFound(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	if _, err := store.GetLatestBySession(context.Background(), "empty"); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ─── SetConclusion ──────────────────────────────────────────────────────────

func TestPGStateSetConclusion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 64)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.SetConclusion(ctx, task.ID, "最终结论"); err != nil {
		t.Fatalf("set conclusion: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Conclusion != "最终结论" {
		t.Fatalf("conclusion = %q", got.Conclusion)
	}
	if got.Version != in.Version+1 {
		t.Fatalf("version = %d, want %d", got.Version, in.Version+1)
	}

	// CommitLog 应有一条 set_conclusion 记录。
	var (
		op      string
		content string
		version int64
	)
	err = db.QueryRowContext(ctx, `
		SELECT op, content, version FROM research_commit_log
		WHERE state_id = $1 ORDER BY seq DESC LIMIT 1`, got.ID).Scan(&op, &content, &version)
	if err != nil {
		t.Fatalf("read commit log: %v", err)
	}
	if op != string(researchstate.OpSetConclusion) {
		t.Fatalf("op = %q, want %q", op, researchstate.OpSetConclusion)
	}
	if content != "最终结论" {
		t.Fatalf("content = %q", content)
	}
	if version != got.Version {
		t.Fatalf("commit log version = %d, state version = %d", version, got.Version)
	}
}

func TestPGStateSetConclusionNotFound(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	err := store.SetConclusion(context.Background(), "nope", "x")
	if !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ─── 提交门控 ───────────────────────────────────────────────────────────────

func TestPGStateCommitGate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 65)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 阶段 3 的 Commit 未实现，但门控必须先于实现生效。
	err := store.Commit(ctx, task.ID, researchstate.CommitInput{})
	if !errors.Is(err, researchstate.ErrNotImplemented) {
		t.Fatalf("open gate: want ErrNotImplemented, got %v", err)
	}

	store.StopCommits(ctx, task.ID)
	err = store.Commit(ctx, task.ID, researchstate.CommitInput{})
	if !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("closed gate: want ErrNotAccepting, got %v", err)
	}

	store.ResumeCommits(ctx, task.ID)
	err = store.Commit(ctx, task.ID, researchstate.CommitInput{})
	if !errors.Is(err, researchstate.ErrNotImplemented) {
		t.Fatalf("reopened gate: want ErrNotImplemented, got %v", err)
	}
}

func TestPGStateGateSelfHealing(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)
	ctx := context.Background()

	// 对不存在的 Task 关闭门控也必须"成功"（派生状态自愈语义）。
	store.StopCommits(ctx, "no-such-task")
	if err := store.Commit(ctx, "no-such-task", researchstate.CommitInput{}); !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("want ErrNotAccepting, got %v", err)
	}
	store.ResumeCommits(ctx, "no-such-task")
	if err := store.Commit(ctx, "no-such-task", researchstate.CommitInput{}); !errors.Is(err, researchstate.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
}

// ─── 未实现的方法必须显式报错 ───────────────────────────────────────────────

func TestPGStateUnimplementedMethodsFailLoudly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := NewResearchStateStore(db)

	checks := []struct {
		name string
		err  error
	}{
		{"RevokeResult", func() error {
			_, err := store.RevokeResult(ctx, "t", "a", "r")
			return err
		}()},
		{"AppendResult", store.AppendResult(ctx, "t", researchstate.CommitInput{})},
		{"AccumulateInterruptedContext", store.AccumulateInterruptedContext(ctx, "t", "x")},
		{"AppendMessage", store.AppendMessage(ctx, "t", researchstate.RoleUser, "x")},
	}
	for _, c := range checks {
		if !errors.Is(c.err, researchstate.ErrNotImplemented) {
			t.Fatalf("%s: want ErrNotImplemented, got %v", c.name, c.err)
		}
	}
}

// ─── 删除 ───────────────────────────────────────────────────────────────────

func TestPGStateDeleteBySession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	store := NewResearchStateStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	sess := &domain.Session{ID: sessionID(66), CreatedAt: base, UpdatedAt: base}
	if err := NewSessionStore(db).Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	task := &domain.Task{ID: taskID(66), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
	if err := NewTaskStore(db).Create(ctx, task); err != nil {
		t.Fatalf("task: %v", err)
	}
	st := &researchstate.State{
		ID: stateIDFromTask(task.ID), TaskID: task.ID, SessionID: sess.ID,
		Ordinal: 1, CreatedAt: base, UpdatedAt: base,
	}
	if err := store.Create(ctx, st); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 关闭门控后删除，门控记录也应被清理。
	store.StopCommits(ctx, task.ID)
	if err := store.DeleteBySession(ctx, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, task.ID); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("state should be deleted, got %v", err)
	}
	// 门控记录已清理 → 重新允许提交（会走到未实现的 Commit）。
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{}); !errors.Is(err, researchstate.ErrNotImplemented) {
		t.Fatalf("gate should be forgotten, got %v", err)
	}
}

func TestPGStateDeleteBySessionNotFound(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	err := store.DeleteBySession(context.Background(), "no-session")
	if !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ─── 级联 ───────────────────────────────────────────────────────────────────

// TestPGStateCascadeOnTaskDelete 验证删除 Task 级联删除 ResearchState 与 commit_log。
func TestPGStateCascadeOnTaskDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 67)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetConclusion(ctx, task.ID, "结论"); err != nil {
		t.Fatalf("conclusion: %v", err)
	}

	if err := NewTaskStore(db).Delete(ctx, task.ID); err != nil {
		t.Fatalf("task delete: %v", err)
	}

	if _, err := store.Get(ctx, task.ID); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("state should be cascade-deleted, got %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM research_commit_log WHERE state_id = $1`, in.ID).Scan(&n); err != nil {
		t.Fatalf("count commit log: %v", err)
	}
	if n != 0 {
		t.Fatalf("commit log should be cascade-deleted, got %d rows", n)
	}
}
