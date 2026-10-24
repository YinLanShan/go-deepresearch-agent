package researchstate

import (
	"strings"
	"testing"
)

// TestBuildContext 验证上下文模板的分区结构：
// 前序节点按序完整展开（需求 / Result / 最终结论 / 过程 Finding），
// 当前节点独立分区，并带上节点序号。
func TestBuildContext(t *testing.T) {
	st := New("id1", "t2", "s1", 2)

	ctx := st.BuildContext()
	for _, want := range []string{
		"# 前序研究节点",
		"# 当前研究节点（第 2 个）",
		"【用户需求（按时间顺序）】",
		"研究产物 Result（第 2 个节点的证据）",
		"最终结论 Conclusion（第 2 个节点的结论）",
		"过程输出 Finding（第 2 个节点的推理过程）",
		"# 当前任务与决策要求",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
	// 未结束时 Conclusion 应显示占位，且不再出现"被中断上下文"分区
	if !strings.Contains(ctx, "（尚未结束研究，暂无结论）") {
		t.Fatalf("unfinished node should mark empty conclusion:\n%s", ctx)
	}
	if strings.Contains(ctx, "被中断的上下文原文") {
		t.Fatalf("interrupted context must not be a standalone section:\n%s", ctx)
	}

	// 追加研究产物（含未返回结果的 Result）、过程输出、结论与前序节点。
	st.Results = append(st.Results,
		Result{Turn: 1, Content: "证据A", Status: ResultCommitted},
		Result{Turn: 2, Content: "证据B", Status: ResultPending},
	)
	st.Findings = append(st.Findings, Finding{Turn: 1, Content: "过程A"})
	st.Conclusion = "本节点的最终结论"
	st.Requirements = []string{"需求一", "需求二"}
	st.RelatedNodes = append(st.RelatedNodes, RelatedNode{
		TaskID:       "t1",
		Ordinal:      1,
		Requirements: []string{"前序需求"},
		Results:      []Result{{Turn: 1, Content: "前序证据", Status: ResultCommitted}},
		Findings:     []Finding{{Turn: 1, Content: "前序过程"}},
		Conclusion:   "前序结论",
	})

	ctx = st.BuildContext()
	for _, want := range []string{
		"## 第 1 个研究节点",             // 前序节点分区标题
		"前序需求",                     // 前序节点需求
		"前序证据",                     // 前序节点 Result
		"前序过程",                     // 前序节点 Finding
		"前序结论",                     // 前序节点 Conclusion
		"证据A",                      // 当前节点 Result
		"证据B " + pendingResultMark, // 未返回结果的 Result 需标注
		"过程A",                      // 当前节点 Finding
		"本节点的最终结论",                 // 当前节点 Conclusion
		"1. 需求一",                   // 需求按时间顺序编号
		"2. 需求二",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
}

// TestBuildContextNoRelated 验证无前序节点与无需求时的分区占位。
func TestBuildContextNoRelated(t *testing.T) {
	st := New("id1", "t1", "s1", 1)
	ctx := st.BuildContext()
	if !strings.Contains(ctx, "（无前序节点）") {
		t.Fatalf("should mark no related nodes:\n%s", ctx)
	}
	if !strings.Contains(ctx, "（尚未提供，等待用户输入）") {
		t.Fatalf("should mark empty requirements:\n%s", ctx)
	}
	if !strings.Contains(ctx, "# 当前研究节点（第 1 个）") {
		t.Fatalf("should mark current node ordinal:\n%s", ctx)
	}
}

// TestNewWithRelated 验证新节点携带前序节点的完整快照（而非摘要）。
func TestNewWithRelated(t *testing.T) {
	prev := New("id0", "t1", "s1", 1)
	prev.Requirements = []string{"上一个需求"}
	prev.Results = append(prev.Results, Result{Turn: 1, Content: "上一轮证据", Status: ResultCommitted})
	prev.Findings = append(prev.Findings, Finding{Turn: 1, Content: "上一轮过程"})
	prev.Conclusion = "上一个结论"

	st := NewWithRelated("id1", "t2", "s1", 2, []RelatedNode{prev.Snapshot()})

	if st.Ordinal != 2 {
		t.Fatalf("ordinal = %d", st.Ordinal)
	}
	if len(st.RelatedNodes) != 1 {
		t.Fatalf("related nodes = %+v", st.RelatedNodes)
	}

	node := st.RelatedNodes[0]
	if node.Ordinal != 1 || node.TaskID != "t1" {
		t.Fatalf("node identity = %+v", node)
	}
	if len(node.Requirements) != 1 || node.Requirements[0] != "上一个需求" {
		t.Fatalf("node requirements = %+v", node.Requirements)
	}
	if len(node.Results) != 1 || node.Results[0].Content != "上一轮证据" {
		t.Fatalf("node results = %+v", node.Results)
	}
	if len(node.Findings) != 1 || node.Findings[0].Content != "上一轮过程" {
		t.Fatalf("node findings = %+v", node.Findings)
	}
	if node.Conclusion != "上一个结论" {
		t.Fatalf("node conclusion = %q", node.Conclusion)
	}
}

// TestAddRequirement 验证需求按时间顺序累积（复数语义）。
func TestAddRequirement(t *testing.T) {
	st := New("id1", "t1", "s1", 1)
	if !st.AddRequirement("  第一条  ") {
		t.Fatal("first requirement should be accepted")
	}
	if !st.AddRequirement("第二条") {
		t.Fatal("second requirement should be accepted")
	}
	if st.AddRequirement("   ") {
		t.Fatal("blank requirement should be ignored")
	}
	if len(st.Requirements) != 2 {
		t.Fatalf("requirements = %+v", st.Requirements)
	}
	if st.Requirements[0] != "第一条" || st.Requirements[1] != "第二条" {
		t.Fatalf("requirements order = %+v", st.Requirements)
	}
}

// TestSummaryPrefersConclusion 验证 Summary 优先返回 Conclusion。
func TestSummaryPrefersConclusion(t *testing.T) {
	st := New("id1", "t1", "s1", 1)
	st.Findings = append(st.Findings, Finding{Turn: 1, Content: "过程"})
	if got := st.Summary(); !strings.Contains(got, "过程") {
		t.Fatalf("summary without conclusion = %q", got)
	}
	st.Conclusion = "最终结论"
	if got := st.Summary(); got != "最终结论" {
		t.Fatalf("summary should prefer conclusion, got %q", got)
	}
}
