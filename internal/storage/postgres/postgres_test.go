package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// testDB 返回一个已迁移的数据库连接。
//
// 集成测试通过环境变量开启（默认跳过，避免本地/CI 无 PG 时失败）：
//
//	DRA_TEST_PG_DSN="postgres://postgres:pass@127.0.0.1:5432/dra_test?sslmode=disable"
//	go test ./internal/storage/postgres/ -v
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DRA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DRA_TEST_PG_DSN not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	db, err := Open(ctx, Config{DSN: dsn, MaxOpen: 5, MaxIdle: 2})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 每次测试前清空业务表（顺序无关，CASCADE 处理外键）。
	if _, err := db.ExecContext(ctx,
		`TRUNCATE sessions, tasks, run_tasks RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func newSession(id string) *domain.Session {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Session{ID: id, Title: "t-" + id, CreatedAt: now, UpdatedAt: now}
}

// ─── SessionStore ───────────────────────────────────────────────────────────

func TestPGSessionRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := NewSessionStore(db)

	in := newSession("10000000000000000000000000000001")
	if err := s.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != in.Title {
		t.Fatalf("title = %q, want %q", got.Title, in.Title)
	}
	if !got.CreatedAt.UTC().Equal(in.CreatedAt.UTC()) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt.UTC(), in.CreatedAt.UTC())
	}
}

func TestPGSessionCreateDuplicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := NewSessionStore(db)

	in := newSession("10000000000000000000000000000002")
	if err := s.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := s.Create(ctx, in)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
}

func TestPGSessionCreateInvalidArgument(t *testing.T) {
	db := testDB(t)
	s := NewSessionStore(db)

	if err := s.Create(context.Background(), &domain.Session{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if err := s.Create(context.Background(), nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
}

func TestPGSessionNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := NewSessionStore(db)

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("delete: want ErrNotFound, got %v", err)
	}
}

func TestPGSessionListOrdered(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := NewSessionStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		sess := &domain.Session{
			ID:        sessionID(i),
			Title:     "s",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base,
		}
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	out, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].CreatedAt.After(out[i].CreatedAt) {
			t.Fatal("sessions should be ordered by created_at asc")
		}
	}
}

// ─── TaskStore ──────────────────────────────────────────────────────────────

func TestPGTaskRoundTripAndCascade(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	sess := newSession("10000000000000000000000000000010")
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("session create: %v", err)
	}

	task := &domain.Task{
		ID:          "20000000000000000000000000000010",
		SessionID:   sess.ID,
		Title:       "研究任务",
		Description: "描述",
		Enabled:     true,
		CreatedAt:   sess.CreatedAt,
		UpdatedAt:   sess.UpdatedAt,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("task create: %v", err)
	}

	got, err := tasks.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("task get: %v", err)
	}
	if got.Title != task.Title || !got.Enabled {
		t.Fatalf("unexpected task: %+v", got)
	}

	// 删除 Session 应级联删除 Task。
	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("session delete: %v", err)
	}
	if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("task should be cascade-deleted, got %v", err)
	}
}

func TestPGTaskUpdateNotFound(t *testing.T) {
	db := testDB(t)
	tasks := NewTaskStore(db)

	err := tasks.Update(context.Background(), &domain.Task{
		ID: "20000000000000000000000000000011",
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPGTaskListBySession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	sess := newSession("10000000000000000000000000000012")
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	other := newSession("10000000000000000000000000000013")
	if err := sessions.Create(ctx, other); err != nil {
		t.Fatalf("session: %v", err)
	}

	for i := 0; i < 2; i++ {
		task := &domain.Task{
			ID:        taskID(i),
			SessionID: sess.ID,
			Title:     "t",
			CreatedAt: sess.CreatedAt.Add(time.Duration(i) * time.Second),
			UpdatedAt: sess.UpdatedAt,
		}
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// 另一个 Session 的 Task 不应出现。
	if err := tasks.Create(ctx, &domain.Task{
		ID: "20000000000000000000000000000020", SessionID: other.ID,
		Title: "other", CreatedAt: other.CreatedAt, UpdatedAt: other.UpdatedAt,
	}); err != nil {
		t.Fatalf("create other: %v", err)
	}

	out, err := tasks.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
}

// ─── RunTaskStore ───────────────────────────────────────────────────────────

func TestPGRunTaskRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)
	runs := NewRunTaskStore(db)

	sess := newSession("10000000000000000000000000000030")
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	task := &domain.Task{
		ID:        "20000000000000000000000000000030",
		SessionID: sess.ID,
		Title:     "t",
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("task: %v", err)
	}

	rt := &domain.RunTask{
		ID:        "30000000000000000000000000000030",
		TaskID:    task.ID,
		Status:    domain.RunStatusRunning,
		Turn:      1,
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
	}
	if err := runs.Create(ctx, rt); err != nil {
		t.Fatalf("run create: %v", err)
	}

	got, err := runs.Get(ctx, rt.ID)
	if err != nil {
		t.Fatalf("run get: %v", err)
	}
	if got.Status != domain.RunStatusRunning || got.Turn != 1 {
		t.Fatalf("unexpected run: %+v", got)
	}

	byTask, err := runs.GetByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get by task: %v", err)
	}
	if byTask.ID != rt.ID {
		t.Fatalf("GetByTask id = %s, want %s", byTask.ID, rt.ID)
	}

	// Update
	got.Status = domain.RunStatusFinished
	got.Turn = 2
	if err := runs.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := runs.Get(ctx, rt.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Status != domain.RunStatusFinished || again.Turn != 2 {
		t.Fatalf("update not applied: %+v", again)
	}

	// 删除 Task 应级联删除 RunTask。
	if err := tasks.Delete(ctx, task.ID); err != nil {
		t.Fatalf("task delete: %v", err)
	}
	if _, err := runs.Get(ctx, rt.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("run task should be cascade-deleted, got %v", err)
	}
}

func TestPGRunTaskExistsForTask(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)
	runs := NewRunTaskStore(db)

	sess := newSession("10000000000000000000000000000031")
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("session: %v", err)
	}
	task := &domain.Task{ID: "20000000000000000000000000000031", SessionID: sess.ID,
		CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("task: %v", err)
	}

	first := &domain.RunTask{ID: "30000000000000000000000000000031", TaskID: task.ID,
		Status: domain.RunStatusRunning, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt}
	if err := runs.Create(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// 同一 Task 的第二个 RunTask → ErrRunTaskExists
	second := &domain.RunTask{ID: "30000000000000000000000000000032", TaskID: task.ID,
		Status: domain.RunStatusRunning, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt}
	err := runs.Create(ctx, second)
	if !errors.Is(err, storage.ErrRunTaskExists) {
		t.Fatalf("want ErrRunTaskExists, got %v", err)
	}
	// 它同时也是 ErrAlreadyExists（特例关系）
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("ErrRunTaskExists should also match ErrAlreadyExists, got %v", err)
	}
}

func TestPGRunTaskDuplicateID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)
	runs := NewRunTaskStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	var taskIDs []string
	for i := 0; i < 2; i++ {
		sess := &domain.Session{ID: sessionID(40 + i), CreatedAt: base, UpdatedAt: base}
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		task := &domain.Task{ID: taskID(40 + i), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
		taskIDs = append(taskIDs, task.ID)
	}

	rt := &domain.RunTask{ID: "30000000000000000000000000000040", TaskID: taskIDs[0],
		Status: domain.RunStatusRunning, CreatedAt: base, UpdatedAt: base}
	if err := runs.Create(ctx, rt); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 同 ID、不同 Task → 主键冲突 → ErrAlreadyExists（而非 ErrRunTaskExists）
	dup := &domain.RunTask{ID: rt.ID, TaskID: taskIDs[1],
		Status: domain.RunStatusRunning, CreatedAt: base, UpdatedAt: base}
	err := runs.Create(ctx, dup)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
	if errors.Is(err, storage.ErrRunTaskExists) {
		t.Fatal("duplicate ID should not be reported as ErrRunTaskExists")
	}
}

func TestPGRunTaskListOrdered(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)
	runs := NewRunTaskStore(db)

	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		sess := &domain.Session{ID: sessionID(50 + i), CreatedAt: base, UpdatedAt: base}
		if err := sessions.Create(ctx, sess); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		task := &domain.Task{ID: taskID(50 + i), SessionID: sess.ID, CreatedAt: base, UpdatedAt: base}
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
		rt := &domain.RunTask{
			ID:        runTaskID(i),
			TaskID:    task.ID,
			Status:    domain.RunStatusRunning,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			UpdatedAt: base,
		}
		if err := runs.Create(ctx, rt); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	out, err := runs.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].CreatedAt.After(out[i].CreatedAt) {
			t.Fatal("run tasks should be ordered by created_at asc")
		}
	}
}

// ─── 辅助 ───────────────────────────────────────────────────────────────────

// sessionID / taskID / runTaskID / assignmentID / workID 生成 32 位十六进制 ID
// （满足 CHECK (length(id) = 32) 约束）。
//
// 用不同的首字符区分实体类型，避免跨表 ID 冲突；
// 末尾 8 位是补零的序号，保证 i=0 与 i=1 不会生成相同字符串。
func sessionID(i int) string    { return hexID('1', i) }
func taskID(i int) string       { return hexID('2', i) }
func runTaskID(i int) string    { return hexID('3', i) }
func assignmentID(i int) string { return hexID('4', i) }
func workID(i int) string       { return hexID('5', i) }

func hexID(prefix byte, i int) string {
	const width = 32
	const seqWidth = 8
	buf := make([]byte, width)
	for j := range buf {
		buf[j] = prefix
	}
	digits := "0123456789abcdef"
	// 把 i 写成 seqWidth 位十六进制，右对齐填充到末尾。
	for j := 0; j < seqWidth; j++ {
		buf[width-1-j] = digits[(i>>(4*j))&0xF]
	}
	return string(buf)
}
