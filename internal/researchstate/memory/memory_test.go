package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	st := researchstate.New("id1", "t1", "s1", "需求")
	if err := s.Create(ctx, st); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Requirement != "需求" {
		t.Fatalf("unexpected state: %+v", got)
	}

	// Commit 两轮研究（Result + Finding）
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "证据1", FindingContent: "结论1"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "证据2", FindingContent: "结论2"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, _ = s.Get(ctx, "t1")
	if len(got.Findings) != 2 || len(got.Results) != 2 || got.Version != 2 {
		t.Fatalf("unexpected state after commit: %+v", got)
	}
	if got.Results[0].Status != researchstate.ResultCommitted {
		t.Fatalf("result should be committed: %+v", got.Results[0])
	}

	// 累积被中断的上下文
	if err := s.AccumulateInterruptedContext(ctx, "t1", "部分A"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}
	if err := s.AccumulateInterruptedContext(ctx, "t1", "部分B"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}
	got, _ = s.Get(ctx, "t1")
	if got.InterruptedContext != "部分A部分B" {
		t.Fatalf("interrupted context should accumulate, got %q", got.InterruptedContext)
	}

	// 删除整个 Session 的研究状态
	if err := s.DeleteBySession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteBySession: %v", err)
	}
	if _, err := s.Get(ctx, "t1"); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "x", FindingContent: "y"}); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("commit on missing should return ErrNotFound, got %v", err)
	}
}

// TestGetLatestBySession 验证按 Session 取最近一份研究状态。
func TestGetLatestBySession(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", "需求1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, researchstate.New("id2", "t2", "s1", "需求2")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	latest, err := s.GetLatestBySession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetLatestBySession: %v", err)
	}
	if latest.TaskID != "t2" {
		t.Fatalf("want latest task t2, got %q", latest.TaskID)
	}
}

// TestCommitGate 验证提交门控：中止后拒绝新 Commit，恢复后重新接受。
func TestCommitGate(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", "需求")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "证据", FindingContent: "结论"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// 中止：关闭门控
	if err := s.StopCommits(ctx, "t1"); err != nil {
		t.Fatalf("StopCommits: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "x", FindingContent: "y"}); !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("want ErrNotAccepting, got %v", err)
	}

	// 恢复：重新接受
	if err := s.ResumeCommits(ctx, "t1"); err != nil {
		t.Fatalf("ResumeCommits: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{ResultContent: "证据2", FindingContent: "结论2"}); err != nil {
		t.Fatalf("Commit after resume: %v", err)
	}
}
