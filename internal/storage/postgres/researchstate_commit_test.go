package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// ─── Commit ─────────────────────────────────────────────────────────────────

func TestPGStateCommitResultsAndFinding(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 80)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: assignmentID(1), Content: "证据 A"},
			{AssignmentID: assignmentID(2), Content: "证据 B"},
			{AssignmentID: assignmentID(3), Content: "   "}, // 空白应被跳过
		},
		FindingContent: "本轮过程输出",
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2 (blank skipped)", len(got.Results))
	}
	if got.Results[0].Content != "证据 A" || got.Results[1].Content != "证据 B" {
		t.Fatalf("unexpected results: %+v", got.Results)
	}
	for _, r := range got.Results {
		if r.Turn != 1 {
			t.Fatalf("result turn = %d, want 1", r.Turn)
		}
		if r.Status != researchstate.ResultCommitted {
			t.Fatalf("result status = %s, want committed", r.Status)
		}
		if r.ID == "" {
			t.Fatal("result id should be generated")
		}
	}
	if len(got.Findings) != 1 || got.Findings[0].Content != "本轮过程输出" {
		t.Fatalf("unexpected findings: %+v", got.Findings)
	}
	if got.Findings[0].Turn != 1 {
		t.Fatalf("finding turn = %d, want 1", got.Findings[0].Turn)
	}
	// Version 只自增一次，与该批 Result 条数无关。
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Version)
	}
	// CommitLog：2 条 result + 1 条 finding。
	if len(got.CommitLog) != 3 {
		t.Fatalf("commit log = %d, want 3: %+v", len(got.CommitLog), got.CommitLog)
	}
	if got.CommitLog[0].Op != researchstate.OpCommitResult {
		t.Fatalf("first op = %s", got.CommitLog[0].Op)
	}
	if got.CommitLog[2].Op != researchstate.OpCommitFinding {
		t.Fatalf("last op = %s", got.CommitLog[2].Op)
	}
	// Seq 应按 state 内部顺序从 1 开始。
	for i, rec := range got.CommitLog {
		if rec.Seq != int64(i+1) {
			t.Fatalf("commit log[%d].Seq = %d, want %d", i, rec.Seq, i+1)
		}
	}
}

// TestPGStateCommitTurnFromFindings 验证 turn 来自 Findings 数量而非 Results。
func TestPGStateCommitTurnFromFindings(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 81)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 第一轮：2 条 Result + 1 条 Finding → turn 1
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: assignmentID(1), Content: "r1"},
			{AssignmentID: assignmentID(2), Content: "r2"},
		},
		FindingContent: "f1",
	}); err != nil {
		t.Fatalf("commit 1: %v", err)
	}

	// 第二轮：1 条 Result + 1 条 Finding → turn 2（若按 Results 数算会得到 turn 4）
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results:        []researchstate.ResultInput{{AssignmentID: assignmentID(3), Content: "r3"}},
		FindingContent: "f2",
	}); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(got.Findings))
	}
	if got.Findings[1].Turn != 2 {
		t.Fatalf("second finding turn = %d, want 2", got.Findings[1].Turn)
	}
	// r3 属于 turn 2。
	var r3 researchstate.Result
	for _, r := range got.Results {
		if r.Content == "r3" {
			r3 = r
		}
	}
	if r3.Turn != 2 {
		t.Fatalf("r3 turn = %d, want 2 (turn derives from findings count)", r3.Turn)
	}
}

// TestPGStateCommitClearsInterruptedContext 验证提交后清空中断原文。
func TestPGStateCommitClearsInterruptedContext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 82)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.AccumulateInterruptedContext(ctx, task.ID, "被中断的原文"); err != nil {
		t.Fatalf("accumulate: %v", err)
	}

	before, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.InterruptedContext != "被中断的原文" {
		t.Fatalf("interrupted = %q", before.InterruptedContext)
	}

	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		FindingContent: "并入后的过程输出",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.InterruptedContext != "" {
		t.Fatalf("interrupted context should be cleared, got %q", after.InterruptedContext)
	}
}

func TestPGStateCommitNotFound(t *testing.T) {
	db := testDB(t)
	store := NewResearchStateStore(db)

	err := store.Commit(context.Background(), "nope", researchstate.CommitInput{})
	if !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ─── RevokeResult ───────────────────────────────────────────────────────────

func TestPGStateRevokeResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 83)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	target := assignmentID(1)
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: target, Content: "将被撤销"},
			{AssignmentID: assignmentID(2), Content: "保留"},
		},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	n, err := store.RevokeResult(ctx, task.ID, target, "用户撤销")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].Content != "保留" {
		t.Fatalf("unexpected results after revoke: %+v", got.Results)
	}
	// 版本：commit 1 次 + revoke 1 次 = 2
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Version)
	}
	// CommitLog 应含一条 revoke 记录，且保留被撤销的原文。
	var revokeRec *researchstate.CommitRecord
	for i := range got.CommitLog {
		if got.CommitLog[i].Op == researchstate.OpRevokeResult {
			revokeRec = &got.CommitLog[i]
		}
	}
	if revokeRec == nil {
		t.Fatalf("revoke record missing: %+v", got.CommitLog)
	}
	if revokeRec.Content != "将被撤销" {
		t.Fatalf("revoke record content = %q (原文应保留)", revokeRec.Content)
	}
	if revokeRec.Reason != "用户撤销" {
		t.Fatalf("revoke reason = %q", revokeRec.Reason)
	}
	if revokeRec.AssignmentID != target {
		t.Fatalf("revoke assignment = %q", revokeRec.AssignmentID)
	}
}

// TestPGStateRevokeResultNotMatchedNoVersionBump 验证未命中时不改版本。
func TestPGStateRevokeResultNotMatchedNoVersionBump(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 84)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: assignmentID(1), Content: "x"}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	before, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// 撤销一个不存在的 Assignment。
	n, err := store.RevokeResult(ctx, task.ID, assignmentID(99), "无")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 0 {
		t.Fatalf("revoked = %d, want 0", n)
	}

	after, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Version != before.Version {
		t.Fatalf("version should not change: %d -> %d", before.Version, after.Version)
	}
	if len(after.CommitLog) != len(before.CommitLog) {
		t.Fatalf("commit log should not change: %d -> %d",
			len(before.CommitLog), len(after.CommitLog))
	}
}

// TestPGStateRevokeResultIgnoresGate 验证撤销不受提交门控限制。
func TestPGStateRevokeResultIgnoresGate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 85)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	target := assignmentID(1)
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: target, Content: "x"}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 研究已结束（门控关闭），仍应允许撤销已提交结果。
	store.StopCommits(ctx, task.ID)
	n, err := store.RevokeResult(ctx, task.ID, target, "结束后撤销")
	if err != nil {
		t.Fatalf("revoke should ignore gate: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
}

func TestPGStateRevokeResultEmptyAssignmentID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 86)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := store.RevokeResult(ctx, task.ID, "", "空")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 0 {
		t.Fatalf("revoked = %d, want 0", n)
	}
}

// ─── AppendResult ───────────────────────────────────────────────────────────

func TestPGStateAppendResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 87)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 迟到产物：不产生 Finding、不清中断原文、不受门控限制。
	store.StopCommits(ctx, task.ID)
	if err := store.AccumulateInterruptedContext(ctx, task.ID, "原文"); err != nil {
		t.Fatalf("accumulate: %v", err)
	}
	if err := store.AppendResult(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: assignmentID(1), Content: "迟到"}},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].Content != "迟到" {
		t.Fatalf("results = %+v", got.Results)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("findings should stay empty, got %+v", got.Findings)
	}
	if got.InterruptedContext != "原文" {
		t.Fatalf("interrupted context should be untouched, got %q", got.InterruptedContext)
	}
	// Reason 固定为"迟到产物"。
	if len(got.CommitLog) != 1 || got.CommitLog[0].Reason != "迟到产物" {
		t.Fatalf("commit log = %+v", got.CommitLog)
	}
}

// TestPGStateAppendResultTurnSameAsCommit 验证 AppendResult 的 turn 与 Commit 同基准。
//
// Turn 表示"第几轮研究"，来自 Findings 数量 + 1，而不是 Result 条数。
// 迟到产物不产生 Finding，因此同一次补录的多条 Result 共享同一个 Turn；
// 若按 Result 条数递增会出现轮次跳号（Result 数总比轮次多）。
func TestPGStateAppendResultTurnSameAsCommit(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 88)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 第一轮正常提交，产生 1 条 Finding → 下一轮 turn 应为 2。
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results:        []researchstate.ResultInput{{AssignmentID: assignmentID(1), Content: "第一轮证据"}},
		FindingContent: "第一轮过程",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 一次补录两条迟到产物：都属于 turn 2（而不是 2 和 3）。
	if err := store.AppendResult(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: assignmentID(2), Content: "迟到一"},
			{AssignmentID: assignmentID(3), Content: "迟到二"},
		},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var lateTurns []int
	for _, r := range got.Results {
		if strings.HasPrefix(r.Content, "迟到") {
			lateTurns = append(lateTurns, r.Turn)
		}
	}
	if len(lateTurns) != 2 {
		t.Fatalf("late results = %d, want 2: %+v", len(lateTurns), got.Results)
	}
	for i, turn := range lateTurns {
		if turn != 2 {
			t.Fatalf("late result[%d].Turn = %d, want 2 (based on findings, not results)", i, turn)
		}
	}

	// 再来一次补录：仍然属于 turn 2（本轮无新 Finding）。
	if err := store.AppendResult(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: assignmentID(4), Content: "迟到三"}},
	}); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	got, err = store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, r := range got.Results {
		if r.Content == "迟到三" && r.Turn != 2 {
			t.Fatalf("late 3 turn = %d, want 2", r.Turn)
		}
	}
}

// TestPGStateAppendResultTurnSkipsNoNumbers 验证 Result 数量不会造成 turn 跳号。
//
// 这是原缺陷的直接回归测试：一轮内提交多条 Result 后，
// 补录的迟到产物不得被算成 turn 3 / turn 4。
func TestPGStateAppendResultTurnSkipsNoNumbers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 93)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 一轮提交 3 条 Result + 1 条 Finding。
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: assignmentID(1), Content: "a"},
			{AssignmentID: assignmentID(2), Content: "b"},
			{AssignmentID: assignmentID(3), Content: "c"},
		},
		FindingContent: "过程",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 补录迟到产物：应为 turn 2，而不是 len(Results)+1 = 4。
	if err := store.AppendResult(ctx, task.ID, researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: assignmentID(4), Content: "late"}},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var lateTurn int
	for _, r := range got.Results {
		if r.Content == "late" {
			lateTurn = r.Turn
		}
	}
	if lateTurn != 2 {
		t.Fatalf("late turn = %d, want 2 (regression: used to be %d)", lateTurn, 3+1)
	}
}

// ─── AccumulateInterruptedContext ───────────────────────────────────────────

func TestPGStateAccumulateInterruptedContext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 89)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 多次中断应累积到同一字段。
	for _, s := range []string{"第一段", "第二段", "第三段"} {
		if err := store.AccumulateInterruptedContext(ctx, task.ID, s); err != nil {
			t.Fatalf("accumulate %q: %v", s, err)
		}
	}
	// 空文本应被忽略。
	if err := store.AccumulateInterruptedContext(ctx, task.ID, ""); err != nil {
		t.Fatalf("accumulate empty: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.InterruptedContext != "第一段第二段第三段" {
		t.Fatalf("interrupted = %q", got.InterruptedContext)
	}
}

// ─── AppendMessage ──────────────────────────────────────────────────────────

func TestPGStateAppendMessage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 90)
	store := NewResearchStateStore(db)

	if err := store.Create(ctx, newState(task, 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.AppendMessage(ctx, task.ID, researchstate.RoleUser, "用户问题"); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := store.AppendMessage(ctx, task.ID, researchstate.RoleAssistant, "模型回答"); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	// 空内容忽略。
	if err := store.AppendMessage(ctx, task.ID, researchstate.RoleUser, ""); err != nil {
		t.Fatalf("append empty: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.History) != 2 {
		t.Fatalf("history = %d, want 2", len(got.History))
	}
	if got.History[0].Role != researchstate.RoleUser || got.History[0].Content != "用户问题" {
		t.Fatalf("history[0] = %+v", got.History[0])
	}
	if got.History[1].Role != researchstate.RoleAssistant || got.History[1].Content != "模型回答" {
		t.Fatalf("history[1] = %+v", got.History[1])
	}
}

// ─── 级联与完整性 ───────────────────────────────────────────────────────────

// TestPGStateChildTablesCascade 验证四张子表随 Task 删除级联清理。
func TestPGStateChildTablesCascade(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 91)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results:        []researchstate.ResultInput{{AssignmentID: assignmentID(1), Content: "x"}},
		FindingContent: "f",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.AppendMessage(ctx, task.ID, researchstate.RoleUser, "m"); err != nil {
		t.Fatalf("message: %v", err)
	}

	if err := NewTaskStore(db).Delete(ctx, task.ID); err != nil {
		t.Fatalf("task delete: %v", err)
	}

	for _, tbl := range []string{
		"research_results", "research_findings",
		"research_commit_log", "research_messages",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+tbl+` WHERE state_id = $1`, in.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("%s should be cascade-deleted, got %d rows", tbl, n)
		}
	}
}

// TestPGStateFullRoundTrip 验证完整状态（含全部集合）在重新读取后一致。
func TestPGStateFullRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	task := seedTask(t, db, 92)
	store := NewResearchStateStore(db)

	in := newState(task, 1)
	in.Requirements = []string{"需求"}
	in.RelatedNodes = []researchstate.RelatedNode{{TaskID: "prev", Ordinal: 0, Conclusion: "旧"}}
	if err := store.Create(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Commit(ctx, task.ID, researchstate.CommitInput{
		Results:        []researchstate.ResultInput{{AssignmentID: assignmentID(1), Content: "证据"}},
		FindingContent: "过程",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.SetConclusion(ctx, task.ID, "结论"); err != nil {
		t.Fatalf("conclusion: %v", err)
	}
	if err := store.AppendMessage(ctx, task.ID, researchstate.RoleUser, "问题"); err != nil {
		t.Fatalf("message: %v", err)
	}

	got, err := store.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Requirements) != 1 || got.Requirements[0] != "需求" {
		t.Fatalf("requirements = %v", got.Requirements)
	}
	if len(got.RelatedNodes) != 1 || got.RelatedNodes[0].Conclusion != "旧" {
		t.Fatalf("related nodes = %+v", got.RelatedNodes)
	}
	if len(got.Results) != 1 || got.Results[0].Content != "证据" {
		t.Fatalf("results = %+v", got.Results)
	}
	if len(got.Findings) != 1 || got.Findings[0].Content != "过程" {
		t.Fatalf("findings = %+v", got.Findings)
	}
	if got.Conclusion != "结论" {
		t.Fatalf("conclusion = %q", got.Conclusion)
	}
	if len(got.History) != 1 || got.History[0].Content != "问题" {
		t.Fatalf("history = %+v", got.History)
	}
	// commit（1 条 result + 1 条 finding）+ set_conclusion = 3 条提交历史
	if len(got.CommitLog) != 3 {
		t.Fatalf("commit log = %d, want 3: %+v", len(got.CommitLog), got.CommitLog)
	}
	// 版本：commit 1 + set_conclusion 1 = 2
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Version)
	}
}
