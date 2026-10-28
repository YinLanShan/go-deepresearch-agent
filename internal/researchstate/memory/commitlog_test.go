package memory

import (
	"context"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// TestCommitLogRecordsOperations 验证提交历史记录提交、撤销与结论写入。
func TestCommitLogRecordsOperations(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: "a1", Content: "证据一"},
			{AssignmentID: "a2", Content: "证据二"},
		},
		FindingContent: "本轮过程",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if len(st.CommitLog) != 3 {
		t.Fatalf("want 3 commit records (2 results + 1 finding), got %+v", st.CommitLog)
	}
	for i, rec := range st.CommitLog {
		if rec.Seq != int64(i+1) {
			t.Fatalf("record %d seq = %d", i, rec.Seq)
		}
		if rec.Version == 0 {
			t.Fatalf("record %d should carry version", i)
		}
	}
	if st.CommitLog[0].Op != researchstate.OpCommitResult || st.CommitLog[0].AssignmentID != "a1" {
		t.Fatalf("first record = %+v", st.CommitLog[0])
	}
	if st.CommitLog[2].Op != researchstate.OpCommitFinding {
		t.Fatalf("third record = %+v", st.CommitLog[2])
	}

	// 撤销 a1：应新增一条撤销记录，并从 Results 中移除对应条目。
	n, err := s.RevokeResult(ctx, "t1", "a1", "目标写错")
	if err != nil {
		t.Fatalf("RevokeResult: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}

	st, _ = s.Get(ctx, "t1")
	if len(st.Results) != 1 || st.Results[0].AssignmentID != "a2" {
		t.Fatalf("only a2 should remain, got %+v", st.Results)
	}
	if len(st.CommitLog) != 4 {
		t.Fatalf("want 4 commit records, got %d", len(st.CommitLog))
	}
	last := st.CommitLog[3]
	if last.Op != researchstate.OpRevokeResult {
		t.Fatalf("last record op = %s", last.Op)
	}
	if last.AssignmentID != "a1" || last.Content != "证据一" || last.Reason != "目标写错" {
		t.Fatalf("revoke record should keep audit info, got %+v", last)
	}
}

// TestRevokeResultIdempotent 验证重复撤销与撤销不存在的 Assignment 都安全。
func TestRevokeResultIdempotent(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: "a1", Content: "证据"}},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if n, err := s.RevokeResult(ctx, "t1", "a1", "第一次"); err != nil || n != 1 {
		t.Fatalf("first revoke: n=%d err=%v", n, err)
	}
	if n, err := s.RevokeResult(ctx, "t1", "a1", "第二次"); err != nil || n != 0 {
		t.Fatalf("second revoke should be no-op: n=%d err=%v", n, err)
	}
	if n, err := s.RevokeResult(ctx, "t1", "no-such", ""); err != nil || n != 0 {
		t.Fatalf("unknown assignment should be no-op: n=%d err=%v", n, err)
	}
	if n, err := s.RevokeResult(ctx, "no-such-task", "a1", ""); err == nil || n != 0 {
		t.Fatalf("unknown task should return ErrNotFound: n=%d err=%v", n, err)
	}
}

// TestRevokeResultWorksAfterStopCommits 验证已结束研究的合法提交也能被撤销。
func TestRevokeResultWorksAfterStopCommits(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: "a1", Content: "已提交证据"}},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	s.StopCommits(ctx, "t1")

	// 门控已关闭：新提交被拒绝
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		Results: []researchstate.ResultInput{{AssignmentID: "a2", Content: "新证据"}},
	}); err == nil {
		t.Fatal("commit after stop should fail")
	}

	// 但撤销已提交的结果仍然允许
	n, err := s.RevokeResult(ctx, "t1", "a1", "撤回")
	if err != nil || n != 1 {
		t.Fatalf("revoke after stop should succeed: n=%d err=%v", n, err)
	}
	st, _ := s.Get(ctx, "t1")
	if len(st.Results) != 0 {
		t.Fatalf("result should be removed, got %+v", st.Results)
	}
}

// TestCommitKeepsResultsSeparate 验证多条结果各自独立保存（不拼成一个字符串）。
func TestCommitKeepsResultsSeparate(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		Results: []researchstate.ResultInput{
			{AssignmentID: "a1", Content: "结果甲"},
			{AssignmentID: "a2", Content: "结果乙"},
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if len(st.Results) != 2 {
		t.Fatalf("want 2 separate results, got %+v", st.Results)
	}
	if st.Results[0].Content != "结果甲" || st.Results[0].AssignmentID != "a1" {
		t.Fatalf("results[0] = %+v", st.Results[0])
	}
	if st.Results[1].Content != "结果乙" || st.Results[1].AssignmentID != "a2" {
		t.Fatalf("results[1] = %+v", st.Results[1])
	}
	for _, r := range st.Results {
		if r.ID == "" {
			t.Fatal("result should carry id")
		}
	}
}
