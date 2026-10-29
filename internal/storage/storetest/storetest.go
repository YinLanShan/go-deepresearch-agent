// Package storetest 提供存储实现的**契约测试**（conformance tests）。
//
// 目的：让内存实现与 PostgreSQL 实现对同一批语义给出相同结果。
// 两份实现必须都能通过本包的全部测试，否则说明语义出现漂移，
// 而漂移的代价是"换数据库后行为变了"这类极难排查的问题。
//
// 用法（在各实现的测试中）：
//
//	func TestContract(t *testing.T) {
//	    storetest.RunAll(t, myFactory, storetest.Capabilities{...})
//	}
//
// 契约来源：docs/DataModel.md 第 8 节的语义对齐清单。
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// Stores 汇总一次测试所需的全部存储实现。
type Stores struct {
	Sessions    storage.SessionStore
	Tasks       storage.TaskStore
	RunTasks    storage.RunTaskStore
	Assignments storage.AssignmentStore
	Works       storage.WorkStore
	States      researchstate.Store
}

// Factory 为一组全新的、彼此隔离的存储（每个测试调用一次）。
type Factory func(t *testing.T) *Stores

// Capabilities 声明某个实现的已知差异。
//
// 用于"内存实现暂时做不到、但 PG 实现做到了"的边角情况；
// 值为 true 表示**跳过**对应的契约断言。
// 每项都应视为技术债，理想状态是全为 false。
type Capabilities struct {
	// SkipCascade 跳过级联删除相关的断言（内存实现需要手动逐层清理）。
	SkipCascade bool
	// SkipAlreadyExistsOnStateCreate 跳过 ResearchState Create 重复检查
	// （早期内存实现会静默覆盖）。
	SkipAlreadyExistsOnStateCreate bool
}

// RunAll 运行全部契约测试。
func RunAll(t *testing.T, newStores Factory, caps Capabilities) {
	t.Helper()

	t.Run("Session", func(t *testing.T) { testSession(t, newStores) })
	t.Run("Task", func(t *testing.T) { testTask(t, newStores) })
	t.Run("RunTask", func(t *testing.T) { testRunTask(t, newStores) })
	t.Run("Assignment", func(t *testing.T) { testAssignment(t, newStores) })
	t.Run("Work", func(t *testing.T) { testWork(t, newStores) })
	t.Run("StateScalar", func(t *testing.T) { testStateScalar(t, newStores) })
	t.Run("StateCommit", func(t *testing.T) { testStateCommit(t, newStores) })
	t.Run("StateRevoke", func(t *testing.T) { testStateRevoke(t, newStores) })

	if !caps.SkipCascade {
		t.Run("Cascade", func(t *testing.T) { testCascade(t, newStores) })
	}
	if !caps.SkipAlreadyExistsOnStateCreate {
		t.Run("StateAlreadyExists", func(t *testing.T) { testStateAlreadyExists(t, newStores) })
	}
}

// ─── 测试骨架 ───────────────────────────────────────────────────────────────

// fixture 是一套已经建好 Session → Task → RunTask 的环境。
type fixture struct {
	st     *Stores
	sess   *domain.Session
	task   *domain.Task
	run    *domain.RunTask
	newID  func() string
	baseTS time.Time
}

// setup 建好三层依赖，供各契约用例直接使用。
func setup(t *testing.T, newStores Factory) *fixture {
	t.Helper()
	ctx := context.Background()
	st := newStores(t)
	base := time.Now().UTC().Truncate(time.Millisecond)

	sess := &domain.Session{ID: ids.New(), Title: "契约会话", CreatedAt: base, UpdatedAt: base}
	if err := st.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("setup session: %v", err)
	}
	task := &domain.Task{
		ID: ids.New(), SessionID: sess.ID, Title: "契约任务",
		Enabled: true, CreatedAt: base, UpdatedAt: base,
	}
	if err := st.Tasks.Create(ctx, task); err != nil {
		t.Fatalf("setup task: %v", err)
	}
	run := &domain.RunTask{
		ID: ids.New(), TaskID: task.ID, Status: domain.RunStatusRunning,
		CreatedAt: base, UpdatedAt: base,
	}
	if err := st.RunTasks.Create(ctx, run); err != nil {
		t.Fatalf("setup run task: %v", err)
	}
	return &fixture{st: st, sess: sess, task: task, run: run, baseTS: base}
}

// newAssignment 构造一个挂在当前 RunTask 下的 Assignment。
func (f *fixture) newAssignment() *domain.Assignment {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.Assignment{
		ID: ids.New(), SessionID: f.sess.ID, TaskID: f.task.ID, RunTaskID: f.run.ID,
		Goal: "契约目标", Status: domain.AssignmentScheduled,
		CreatedAt: now, UpdatedAt: now,
	}
}

// newState 构造一份挂当前 Task 的研究状态。
func (f *fixture) newState() *researchstate.State {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &researchstate.State{
		ID: ids.New(), TaskID: f.task.ID, SessionID: f.sess.ID, Ordinal: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

// ─── Session ────────────────────────────────────────────────────────────────

func testSession(t *testing.T, newStores Factory) {
	ctx := context.Background()
	st := newStores(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	in := &domain.Session{ID: ids.New(), Title: "标题", CreatedAt: now, UpdatedAt: now}
	if err := st.Sessions.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 往返：字段不能丢。
	got, err := st.Sessions.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != in.ID || got.Title != in.Title {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !got.CreatedAt.UTC().Equal(in.CreatedAt.UTC()) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt.UTC(), in.CreatedAt.UTC())
	}

	// 副本语义：改动返回值不得影响存储。
	got.Title = "被篡改"
	again, err := st.Sessions.Get(ctx, in.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Title != "标题" {
		t.Fatal("Get must return a copy; mutation leaked into the store")
	}

	// 重复创建 → ErrAlreadyExists。
	if err := st.Sessions.Create(ctx, in); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate create: want ErrAlreadyExists, got %v", err)
	}

	// 空参数 → ErrInvalidArgument（不是 ErrNotFound）。
	if err := st.Sessions.Create(ctx, &domain.Session{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("empty id: want ErrInvalidArgument, got %v", err)
	}

	// 不存在 → ErrNotFound。
	if _, err := st.Sessions.Get(ctx, ids.New()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing get: want ErrNotFound, got %v", err)
	}
	if err := st.Sessions.Delete(ctx, ids.New()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing delete: want ErrNotFound, got %v", err)
	}

	// 列表：包含已创建的会话。
	list, err := st.Sessions.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("list = %+v", list)
	}

	// 删除后不再可见。
	if err := st.Sessions.Delete(ctx, in.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Sessions.Get(ctx, in.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

// ─── Task ───────────────────────────────────────────────────────────────────

func testTask(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	// 挂一个 Task 到该会话。
	now := time.Now().UTC().Truncate(time.Millisecond)
	extra := &domain.Task{
		ID: ids.New(), SessionID: f.sess.ID, Title: "第二个任务",
		Description: "描述文本", Enabled: false,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Tasks.Create(ctx, extra); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Tasks.Get(ctx, extra.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != extra.Title || got.Description != extra.Description {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Enabled {
		t.Fatal("enabled should be false")
	}

	// 重复创建 → ErrAlreadyExists。
	if err := st.Tasks.Create(ctx, extra); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}

	// Update 生效。
	got.Enabled = true
	got.UpdatedAt = now.Add(time.Second)
	if err := st.Tasks.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := st.Tasks.Get(ctx, extra.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if !again.Enabled {
		t.Fatal("update not applied")
	}

	// Update 不存在的对象 → ErrNotFound（不得"复活"）。
	ghost := &domain.Task{ID: ids.New(), SessionID: f.sess.ID,
		CreatedAt: now, UpdatedAt: now}
	if err := st.Tasks.Update(ctx, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}

	// ListBySession：只返回本会话的 Task（setup 建了 1 个 + extra = 2）。
	list, err := st.Tasks.ListBySession(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
}

// ─── RunTask ────────────────────────────────────────────────────────────────

func testRunTask(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	// Get / GetByTask 一致。
	byID, err := st.RunTasks.Get(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	byTask, err := st.RunTasks.GetByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get by task: %v", err)
	}
	if byID.ID != byTask.ID {
		t.Fatalf("Get and GetByTask disagree: %s vs %s", byID.ID, byTask.ID)
	}

	// 同一 Task 的第二个 RunTask → ErrRunTaskExists，且同时匹配 ErrAlreadyExists。
	second := &domain.RunTask{
		ID: ids.New(), TaskID: f.task.ID, Status: domain.RunStatusRunning,
		CreatedAt: f.baseTS, UpdatedAt: f.baseTS,
	}
	err = st.RunTasks.Create(ctx, second)
	if !errors.Is(err, storage.ErrRunTaskExists) {
		t.Fatalf("want ErrRunTaskExists, got %v", err)
	}
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatal("ErrRunTaskExists should also match ErrAlreadyExists")
	}

	// 状态更新往返。
	byID.Status = domain.RunStatusFinished
	byID.Turn = 3
	byID.UpdatedAt = f.baseTS.Add(time.Second)
	if err := st.RunTasks.Update(ctx, byID); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := st.RunTasks.Get(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Status != domain.RunStatusFinished || again.Turn != 3 {
		t.Fatalf("update not applied: %+v", again)
	}

	// Update 不存在 → ErrNotFound。
	ghost := &domain.RunTask{ID: ids.New(), TaskID: f.task.ID,
		CreatedAt: f.baseTS, UpdatedAt: f.baseTS}
	if err := st.RunTasks.Update(ctx, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}

	// List 包含当前 RunTask。
	list, err := st.RunTasks.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != f.run.ID {
		t.Fatalf("list = %+v", list)
	}
}

// ─── Assignment ─────────────────────────────────────────────────────────────

func testAssignment(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	a := f.newAssignment()
	if err := st.Assignments.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Assignments.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Goal != a.Goal || got.Status != a.Status {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// 未完成的 Assignment，FinishedAt 必须是零值（不能是 0001-01-01 之类）。
	if !got.FinishedAt.IsZero() {
		t.Fatalf("finished_at should be zero, got %v", got.FinishedAt)
	}

	// 重复 → ErrAlreadyExists。
	if err := st.Assignments.Create(ctx, a); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}
	// 空参数 → ErrInvalidArgument。
	if err := st.Assignments.Create(ctx, &domain.Assignment{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("empty id: want ErrInvalidArgument, got %v", err)
	}

	// 完成后写入 Result 与 FinishedAt。
	got.Status = domain.AssignmentCompleted
	got.Result = "产物"
	got.FinishedAt = f.baseTS.Add(2 * time.Second)
	got.UpdatedAt = got.FinishedAt
	if err := st.Assignments.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := st.Assignments.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Result != "产物" || !again.FinishedAt.UTC().Equal(got.FinishedAt.UTC()) {
		t.Fatalf("update not applied: %+v", again)
	}

	// Update 不存在 → ErrNotFound。
	ghost := f.newAssignment()
	if err := st.Assignments.Update(ctx, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}

	// ListByRunTask 只返回本 RunTask 的。
	a2 := f.newAssignment()
	if err := st.Assignments.Create(ctx, a2); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	list, err := st.Assignments.ListByRunTask(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}

	// DeleteByRunTask 幂等：删 0 行也返回 nil（与业务 Delete 不同）。
	if err := st.Assignments.DeleteByRunTask(ctx, ids.New()); err != nil {
		t.Fatalf("delete by unknown run task should be nil, got %v", err)
	}
	if err := st.Assignments.DeleteByRunTask(ctx, f.run.ID); err != nil {
		t.Fatalf("delete by run task: %v", err)
	}
	if _, err := st.Assignments.Get(ctx, a.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

// ─── Work ───────────────────────────────────────────────────────────────────

func testWork(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	a := f.newAssignment()
	if err := st.Assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	w := &domain.Work{
		ID: ids.New(), AssignmentID: a.ID, SessionID: f.sess.ID,
		TaskID: f.task.ID, RunTaskID: f.run.ID, Goal: "子任务",
		Status: domain.WorkRunning, WorkerID: "w1", Attempt: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Works.Create(ctx, w); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Works.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Constraint 未设置时必须是 nil（不能是空结构体）。
	if got.Constraint != nil {
		t.Fatalf("constraint should be nil, got %+v", got.Constraint)
	}
	if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Fatalf("timestamps should be zero: %v / %v", got.StartedAt, got.FinishedAt)
	}

	// Constraint 往返。
	got.Constraint = &domain.WorkConstraint{
		ToolAllowed: []string{"web_search"},
		TimeLimit:   30 * time.Second,
	}
	got.Status = domain.WorkSucceeded
	got.Result = "完成"
	got.StartedAt = now
	got.FinishedAt = now.Add(time.Second)
	got.UpdatedAt = got.FinishedAt
	if err := st.Works.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := st.Works.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Constraint == nil || len(again.Constraint.ToolAllowed) != 1 {
		t.Fatalf("constraint lost: %+v", again.Constraint)
	}
	if again.Constraint.TimeLimit != 30*time.Second {
		t.Fatalf("time limit = %v", again.Constraint.TimeLimit)
	}
	if !again.StartedAt.UTC().Equal(got.StartedAt.UTC()) {
		t.Fatalf("started_at = %v", again.StartedAt)
	}

	// 重复 → ErrAlreadyExists；Update 缺失 → ErrNotFound。
	if err := st.Works.Create(ctx, w); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}
	ghost := &domain.Work{ID: ids.New(), AssignmentID: a.ID, RunTaskID: f.run.ID,
		CreatedAt: now, UpdatedAt: now}
	if err := st.Works.Update(ctx, ghost); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}

	// DeleteByRunTask 幂等。
	if err := st.Works.DeleteByRunTask(ctx, ids.New()); err != nil {
		t.Fatalf("delete by unknown run task should be nil, got %v", err)
	}
}

// ─── ResearchState：标量 ────────────────────────────────────────────────────

func testStateScalar(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	in := f.newState()
	in.Requirements = []string{"需求一"}
	in.RelatedNodes = []researchstate.RelatedNode{{TaskID: "prev", Ordinal: 0, Conclusion: "旧结论"}}
	if err := st.States.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Requirements) != 1 || got.Requirements[0] != "需求一" {
		t.Fatalf("requirements = %v", got.Requirements)
	}
	if len(got.RelatedNodes) != 1 || got.RelatedNodes[0].Conclusion != "旧结论" {
		t.Fatalf("related nodes = %+v", got.RelatedNodes)
	}

	// 空参数 → ErrInvalidArgument。
	if err := st.States.Create(ctx, &researchstate.State{}); !errors.Is(err, researchstate.ErrInvalidArgument) {
		t.Fatalf("empty state: want ErrInvalidArgument, got %v", err)
	}

	// 不存在 → ErrNotFound。
	if _, err := st.States.Get(ctx, ids.New()); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("missing get: want ErrNotFound, got %v", err)
	}

	// GetLatestBySession 返回唯一那份。
	latest, err := st.States.GetLatestBySession(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.TaskID != f.task.ID {
		t.Fatalf("latest task = %s, want %s", latest.TaskID, f.task.ID)
	}

	// AddRequirement 累积、忽略空白、不去重。
	for _, r := range []string{"需求二", "需求一", "  "} {
		if err := st.States.AddRequirement(ctx, f.task.ID, r); err != nil {
			t.Fatalf("add %q: %v", r, err)
		}
	}
	got, err = st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Requirements) != 3 {
		t.Fatalf("requirements = %v, want 3 (append, no dedup)", got.Requirements)
	}

	// SetConclusion 自增版本并写提交历史。
	before := got.Version
	if err := st.States.SetConclusion(ctx, f.task.ID, "最终结论"); err != nil {
		t.Fatalf("conclusion: %v", err)
	}
	got, err = st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Conclusion != "最终结论" {
		t.Fatalf("conclusion = %q", got.Conclusion)
	}
	if got.Version != before+1 {
		t.Fatalf("version = %d, want %d", got.Version, before+1)
	}
	if len(got.CommitLog) != 1 || got.CommitLog[0].Op != researchstate.OpSetConclusion {
		t.Fatalf("commit log = %+v", got.CommitLog)
	}
}

// ─── ResearchState：Commit ──────────────────────────────────────────────────

func testStateCommit(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	if err := st.States.Create(ctx, f.newState()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 提交门控：关闭后拒绝。
	st.States.StopCommits(ctx, f.task.ID)
	err := st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: "a1", Content: "x"}},
	})
	if !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("closed gate: want ErrNotAccepting, got %v", err)
	}
	st.States.ResumeCommits(ctx, f.task.ID)

	// 正常提交：空白跳过、Version 只涨一次、turn = findings+1。
	err = st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: "a1", Content: "证据一"},
			{AssignmentID: "a2", Content: "证据二"},
			{AssignmentID: "a3", Content: "   "},
		},
		FindingContent: "过程一",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2 (blank skipped)", len(got.Results))
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1 (single bump per commit)", got.Version)
	}
	if len(got.Findings) != 1 || got.Findings[0].Turn != 1 {
		t.Fatalf("findings = %+v", got.Findings)
	}
	for _, r := range got.Results {
		if r.Turn != 1 {
			t.Fatalf("result turn = %d, want 1", r.Turn)
		}
		if r.Status != researchstate.ResultCommitted {
			t.Fatalf("result status = %s", r.Status)
		}
	}
	// 提交历史：2 条 result + 1 条 finding。
	if len(got.CommitLog) != 3 {
		t.Fatalf("commit log = %d, want 3", len(got.CommitLog))
	}

	// 第二轮：turn 应为 2（来自 Findings 数量）。
	if err := st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		Results:        []researchstate.ResultInput{{AssignmentID: "a4", Content: "证据三"}},
		FindingContent: "过程二",
	}); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	got, err = st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var a4Turn int
	for _, r := range got.Results {
		if r.AssignmentID == "a4" {
			a4Turn = r.Turn
		}
	}
	if a4Turn != 2 {
		t.Fatalf("a4 turn = %d, want 2", a4Turn)
	}

	// InterruptedContext 提交后清空。
	if err := st.States.AccumulateInterruptedContext(ctx, f.task.ID, "中断原文"); err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	if err := st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		FindingContent: "过程三",
	}); err != nil {
		t.Fatalf("commit 3: %v", err)
	}
	got, err = st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.InterruptedContext != "" {
		t.Fatalf("interrupted should be cleared, got %q", got.InterruptedContext)
	}

	// AppendMessage 往返。
	if err := st.States.AppendMessage(ctx, f.task.ID, researchstate.RoleUser, "问题"); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if err := st.States.AppendMessage(ctx, f.task.ID, researchstate.RoleAssistant, "回答"); err != nil {
		t.Fatalf("append message: %v", err)
	}
	got, err = st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.History) != 2 || got.History[0].Content != "问题" || got.History[1].Content != "回答" {
		t.Fatalf("history = %+v", got.History)
	}

	// Commit 不存在的状态 → ErrNotFound。
	if err := st.States.Commit(ctx, ids.New(), researchstate.CommitInput{}); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("missing commit: want ErrNotFound, got %v", err)
	}
}

// ─── ResearchState：Revoke ──────────────────────────────────────────────────

func testStateRevoke(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	if err := st.States.Create(ctx, f.newState()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: "target", Content: "将被撤销"},
			{AssignmentID: "keep", Content: "保留"},
		},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	before, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// 撤销命中：返回数量、移除条目、保留原文与原因。
	n, err := st.States.RevokeResult(ctx, f.task.ID, "target", "用户撤销")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
	got, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].Content != "保留" {
		t.Fatalf("results = %+v", got.Results)
	}
	if got.Version != before.Version+1 {
		t.Fatalf("version = %d, want %d", got.Version, before.Version+1)
	}
	var rec *researchstate.CommitRecord
	for i := range got.CommitLog {
		if got.CommitLog[i].Op == researchstate.OpRevokeResult {
			rec = &got.CommitLog[i]
		}
	}
	if rec == nil {
		t.Fatalf("revoke record missing: %+v", got.CommitLog)
	}
	if rec.Content != "将被撤销" || rec.Reason != "用户撤销" {
		t.Fatalf("revoke record = %+v (原文与原因必须保留)", rec)
	}

	// 未命中：返回 0 且不改版本、不写日志。
	mid, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	n, err = st.States.RevokeResult(ctx, f.task.ID, "no-such", "无")
	if err != nil {
		t.Fatalf("revoke miss: %v", err)
	}
	if n != 0 {
		t.Fatalf("revoked = %d, want 0", n)
	}
	after, err := st.States.Get(ctx, f.task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Version != mid.Version || len(after.CommitLog) != len(mid.CommitLog) {
		t.Fatal("revoke miss must not bump version nor write commit log")
	}

	// 空 assignmentID：返回 0，不报错。
	if n, err := st.States.RevokeResult(ctx, f.task.ID, "", "空"); err != nil || n != 0 {
		t.Fatalf("empty assignment: n=%d err=%v", n, err)
	}

	// 门控关闭时仍可撤销（研究已结束也能撤回）。
	st.States.StopCommits(ctx, f.task.ID)
	if _, err := st.States.RevokeResult(ctx, f.task.ID, "keep", "结束后撤销"); err != nil {
		t.Fatalf("revoke should ignore gate: %v", err)
	}
	// AppendResult 同样不受门控限制。
	if err := st.States.AppendResult(ctx, f.task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: "late", Content: "迟到"}},
	}); err != nil {
		t.Fatalf("append should ignore gate: %v", err)
	}

	// 不存在的状态 → ErrNotFound。
	if _, err := st.States.RevokeResult(ctx, ids.New(), "a", "r"); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("missing revoke: want ErrNotFound, got %v", err)
	}
}

// ─── 级联删除（由调用方显式编排）──────────────────────────────────────────
//
// 级联删除在应用层编排（见 api.handleDeleteSession）：
//
//	Task 逐个 Delete → ResearchState.DeleteBySession →
//	Assignment/Work.DeleteByRunTask → Session.Delete
//
// 因此契约测试验证的是**这组显式调用的组合效果**，而不是"删除 Session
// 会不会自动清库"。PostgreSQL 的外键级联是额外的安全网（有专门测试），
// 内存实现没有、也不需要——它依赖调用方按顺序清理。
func testCascade(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	a := f.newAssignment()
	if err := st.Assignments.Create(ctx, a); err != nil {
		t.Fatalf("assignment: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	w := &domain.Work{
		ID: ids.New(), AssignmentID: a.ID, SessionID: f.sess.ID,
		TaskID: f.task.ID, RunTaskID: f.run.ID, Goal: "g",
		Status: domain.WorkRunning, Attempt: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.Works.Create(ctx, w); err != nil {
		t.Fatalf("work: %v", err)
	}
	if err := st.States.Create(ctx, f.newState()); err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := st.States.Commit(ctx, f.task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: a.ID, Content: "证据"}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 按 API 层的编排顺序逐层清理。
	if err := st.Works.DeleteByRunTask(ctx, f.run.ID); err != nil {
		t.Fatalf("delete works: %v", err)
	}
	if err := st.Assignments.DeleteByRunTask(ctx, f.run.ID); err != nil {
		t.Fatalf("delete assignments: %v", err)
	}
	if err := st.States.DeleteBySession(ctx, f.sess.ID); err != nil {
		t.Fatalf("delete states: %v", err)
	}
	if err := st.RunTasks.Delete(ctx, f.run.ID); err != nil {
		t.Fatalf("delete run task: %v", err)
	}
	if err := st.Tasks.Delete(ctx, f.task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if err := st.Sessions.Delete(ctx, f.sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	// 全部消失。
	if _, err := st.Works.Get(ctx, w.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("work should be gone, got %v", err)
	}
	if _, err := st.Assignments.Get(ctx, a.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("assignment should be gone, got %v", err)
	}
	if _, err := st.States.Get(ctx, f.task.ID); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("state should be gone, got %v", err)
	}
	if _, err := st.RunTasks.Get(ctx, f.run.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("run task should be gone, got %v", err)
	}
	if _, err := st.Tasks.Get(ctx, f.task.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("task should be gone, got %v", err)
	}
	if _, err := st.Sessions.Get(ctx, f.sess.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("session should be gone, got %v", err)
	}
}

// ─── ResearchState 重复创建 ─────────────────────────────────────────────────

func testStateAlreadyExists(t *testing.T, newStores Factory) {
	f := setup(t, newStores)
	ctx := context.Background()
	st := f.st

	if err := st.States.Create(ctx, f.newState()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 同一 Task 再建一份 → ErrAlreadyExists（不得静默覆盖）。
	if err := st.States.Create(ctx, f.newState()); !errors.Is(err, researchstate.ErrAlreadyExists) {
		t.Fatalf("duplicate state: want ErrAlreadyExists, got %v", err)
	}
}
