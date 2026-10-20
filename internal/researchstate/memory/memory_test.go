package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	st := researchstate.New("id1", "t1", "s1", 1, "需求")
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

	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "需求1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, researchstate.New("id2", "t2", "s1", 1, "需求2")); err != nil {
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
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "需求")); err != nil {
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

// TestRequirementAndHistory 验证"需求来自首条消息"与对话历史累积。
func TestRequirementAndHistory(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 首条消息写入需求；后续消息不覆盖
	if err := s.SetRequirementIfEmpty(ctx, "t1", "第一条消息"); err != nil {
		t.Fatalf("SetRequirementIfEmpty: %v", err)
	}
	if err := s.SetRequirementIfEmpty(ctx, "t1", "第二条消息"); err != nil {
		t.Fatalf("SetRequirementIfEmpty: %v", err)
	}
	got, _ := s.Get(ctx, "t1")
	if got.Requirement != "第一条消息" {
		t.Fatalf("requirement should come from first message, got %q", got.Requirement)
	}

	// 对话历史累积 user / assistant
	if err := s.AppendMessage(ctx, "t1", researchstate.RoleUser, "第一条消息"); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if err := s.AppendMessage(ctx, "t1", researchstate.RoleAssistant, "模型回答"); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	got, _ = s.Get(ctx, "t1")
	if len(got.History) != 2 {
		t.Fatalf("want 2 history messages, got %+v", got.History)
	}
	if got.History[0].Role != researchstate.RoleUser || got.History[0].Content != "第一条消息" {
		t.Fatalf("history[0] = %+v", got.History[0])
	}
	if got.History[1].Role != researchstate.RoleAssistant || got.History[1].Content != "模型回答" {
		t.Fatalf("history[1] = %+v", got.History[1])
	}

	// Get 返回的是深拷贝：修改返回值不影响内部状态
	got.History = append(got.History, researchstate.Message{Role: researchstate.RoleUser, Content: "外部篡改"})
	again, _ := s.Get(ctx, "t1")
	if len(again.History) != 2 {
		t.Fatalf("store should be isolated from returned copy, got %d", len(again.History))
	}
}

// TestInterruptedContextNotConsumedByCommit 验证 InterruptedContext 不被 Commit 消费：
// Commit 只追加 Result 与 Finding，中断上下文持续保留并进入上下文构建。
func TestInterruptedContextNotConsumedByCommit(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "需求")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.AccumulateInterruptedContext(ctx, "t1", "中断前的推理原文"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{
		ResultContent: "本轮证据", FindingContent: "本轮结论"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if st.InterruptedContext != "中断前的推理原文" {
		t.Fatalf("interrupted context must survive commit, got %q", st.InterruptedContext)
	}
	if !strings.Contains(st.BuildContext(), "中断前的推理原文") {
		t.Fatal("interrupted context should appear in built context")
	}
}

// TestAppendResultOnly 验证迟到产物只追加 Result：
// 不产生 Finding、不动 InterruptedContext、不受提交门控限制。
func TestAppendResultOnly(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "需求")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.AccumulateInterruptedContext(ctx, "t1", "中断原文"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}

	// 模拟任务已中止：门控关闭
	if err := s.StopCommits(ctx, "t1"); err != nil {
		t.Fatalf("StopCommits: %v", err)
	}

	// 迟到产物仍可追加，且不产生 Finding
	if err := s.AppendResult(ctx, "t1", researchstate.CommitInput{ResultContent: "迟到的证据"}); err != nil {
		t.Fatalf("AppendResult: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if len(st.Results) != 1 || st.Results[0].Content != "迟到的证据" {
		t.Fatalf("results = %+v", st.Results)
	}
	if len(st.Findings) != 0 {
		t.Fatalf("append result must not create findings, got %+v", st.Findings)
	}
	if st.InterruptedContext != "中断原文" {
		t.Fatalf("interrupted context must be untouched, got %q", st.InterruptedContext)
	}
	if st.Version != 1 {
		t.Fatalf("version should increase, got %d", st.Version)
	}
}
