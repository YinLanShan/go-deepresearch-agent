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

	st := researchstate.New("id1", "t1", "s1", 1)
	if err := s.Create(ctx, st); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TaskID != "t1" || len(got.Requirements) != 0 {
		t.Fatalf("unexpected state: %+v", got)
	}

	// Commit 两轮研究（Result + Finding）
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "证据1"}}, FindingContent: "结论1"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "证据2"}}, FindingContent: "结论2"}); err != nil {
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
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "x"}}, FindingContent: "y"}); !errors.Is(err, researchstate.ErrNotFound) {
		t.Fatalf("commit on missing should return ErrNotFound, got %v", err)
	}
}

// TestGetLatestBySession 验证按 Session 取最近一份研究状态。
func TestGetLatestBySession(t *testing.T) {
	ctx := context.Background()
	s := NewStore()

	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, researchstate.New("id2", "t2", "s1", 1)); err != nil {
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
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "证据"}}, FindingContent: "结论"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// 中止：关闭门控
	if err := s.StopCommits(ctx, "t1"); err != nil {
		t.Fatalf("StopCommits: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "x"}}, FindingContent: "y"}); !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("want ErrNotAccepting, got %v", err)
	}

	// 恢复：重新接受
	if err := s.ResumeCommits(ctx, "t1"); err != nil {
		t.Fatalf("ResumeCommits: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "证据2"}}, FindingContent: "结论2"}); err != nil {
		t.Fatalf("Commit after resume: %v", err)
	}
}

// TestRequirementAndHistory 验证需求按时间顺序累积（复数语义）与对话历史累积。
func TestRequirementAndHistory(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 每条用户消息作为一条需求追加，不覆盖
	if err := s.AddRequirement(ctx, "t1", "第一条消息"); err != nil {
		t.Fatalf("AddRequirement: %v", err)
	}
	if err := s.AddRequirement(ctx, "t1", "第二条消息"); err != nil {
		t.Fatalf("AddRequirement: %v", err)
	}
	got, _ := s.Get(ctx, "t1")
	if len(got.Requirements) != 2 ||
		got.Requirements[0] != "第一条消息" || got.Requirements[1] != "第二条消息" {
		t.Fatalf("requirements should accumulate in order, got %+v", got.Requirements)
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

// TestInterruptedContextRenderedBeforeCommit 验证被打断的原文在提交前必须被渲染：
// 否则模型重入时看不到中断前写到哪，等于丢失实际上下文。
func TestInterruptedContextRenderedBeforeCommit(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 初始无中断：不应出现该分区
	if strings.Contains(s.mustGet(t, ctx).BuildContext(), "上一轮被中断的输出") {
		t.Fatal("empty interrupted context should not render a section")
	}

	if err := s.AccumulateInterruptedContext(ctx, "t1", "我正在分析框架 A 的执行模型，"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}
	if err := s.AccumulateInterruptedContext(ctx, "t1", "但还没得出结论"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}

	built := s.mustGet(t, ctx).BuildContext()
	if !strings.Contains(built, "上一轮被中断的输出") {
		t.Fatalf("interrupted context should be rendered:\n%s", built)
	}
	if !strings.Contains(built, "我正在分析框架 A 的执行模型，但还没得出结论") {
		t.Fatalf("interrupted text should appear verbatim:\n%s", built)
	}
}

// mustGet 读取状态，失败即终止测试。
func (s *Store) mustGet(t *testing.T, ctx context.Context) *researchstate.State {
	t.Helper()
	st, err := s.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return st
}

// TestInterruptedContextClearedByCommit 验证 InterruptedContext 是中间产物：
// 其内容已由调用方并入本轮 Finding，Commit 成功后应被清空且不再渲染。
func TestInterruptedContextClearedByCommit(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.AccumulateInterruptedContext(ctx, "t1", "中断前的推理原文"); err != nil {
		t.Fatalf("AccumulateInterruptedContext: %v", err)
	}
	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "中断前的推理原文\n本轮输出"}}, FindingContent: "中断前的推理原文\n本轮输出"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if st.InterruptedContext != "" {
		t.Fatalf("interrupted context should be cleared after commit, got %q", st.InterruptedContext)
	}
	// 原文已并入 Finding，可通过 Finding 追溯
	if !strings.Contains(st.Findings[0].Content, "中断前的推理原文") {
		t.Fatalf("merged finding should contain interrupted text, got %q", st.Findings[0].Content)
	}
	// 清空后不再是独立分区（原文仍在 Finding 里）
	if strings.Contains(st.BuildContext(), "上一轮被中断的输出") {
		t.Fatal("cleared interrupted context must not render a section")
	}
}

// TestSetConclusion 验证 Conclusion 独立存储，不受 Commit 影响。
func TestSetConclusion(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	st, _ := s.Get(ctx, "t1")
	if st.Conclusion != "" {
		t.Fatalf("conclusion should be empty before finish, got %q", st.Conclusion)
	}

	if err := s.Commit(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "过程证据"}}, FindingContent: "过程输出"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	st, _ = s.Get(ctx, "t1")
	if st.Conclusion != "" {
		t.Fatalf("commit must not write conclusion, got %q", st.Conclusion)
	}

	if err := s.SetConclusion(ctx, "t1", "整项研究的最终结论"); err != nil {
		t.Fatalf("SetConclusion: %v", err)
	}
	st, _ = s.Get(ctx, "t1")
	if st.Conclusion != "整项研究的最终结论" {
		t.Fatalf("conclusion = %q", st.Conclusion)
	}
	if !strings.Contains(st.BuildContext(), "整项研究的最终结论") {
		t.Fatal("conclusion should appear in built context")
	}
}

// TestAppendResultOnly 验证迟到产物只追加 Result：
// 不产生 Finding、不动 InterruptedContext、不受提交门控限制。
func TestAppendResultOnly(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
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
	if err := s.AppendResult(ctx, "t1", researchstate.CommitInput{Results: []researchstate.ResultInput{{Content: "迟到的证据"}}}); err != nil {
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
