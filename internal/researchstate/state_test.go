package researchstate

import (
	"strings"
	"testing"
)

// TestBuildContext 验证上下文模板的分区结构：
// 前序节点按序完整展开（需求 / Result / Finding / 中断原文），
// 当前节点独立分区，并带上节点序号。
func TestBuildContext(t *testing.T) {
	st := New("id1", "t2", "s1", 2, "研究某框架的架构与性能")

	ctx := st.BuildContext()
	for _, want := range []string{
		"# 前序研究节点",
		"# 当前研究节点（第 2 个）",
		"研究需求",
		"研究产物 Result（第 2 个节点的证据）",
		"研究结论 Finding（第 2 个节点的知识）",
		"被中断的上下文原文（第 2 个节点的中断记录）",
		"# 当前任务与决策要求",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}

	// 追加研究产物（含未返回结果的 Result）、结论、中断上下文与前序节点。
	st.Results = append(st.Results,
		Result{Turn: 1, Content: "证据A", Status: ResultCommitted},
		Result{Turn: 2, Content: "证据B", Status: ResultPending},
	)
	st.Findings = append(st.Findings, Finding{Turn: 1, Content: "结论A"})
	st.InterruptedContext = "被中断的上下文A"
	st.RelatedNodes = append(st.RelatedNodes, RelatedNode{
		TaskID:             "t1",
		Ordinal:            1,
		Requirement:        "前序需求",
		Results:            []Result{{Turn: 1, Content: "前序证据", Status: ResultCommitted}},
		Findings:           []Finding{{Turn: 1, Content: "前序结论"}},
		InterruptedContext: "前序中断原文",
	})

	ctx = st.BuildContext()
	for _, want := range []string{
		"## 第 1 个研究节点",             // 前序节点分区标题
		"前序需求",                     // 前序节点需求
		"前序证据",                     // 前序节点 Result
		"前序结论",                     // 前序节点 Finding
		"前序中断原文",                   // 前序节点中断上下文
		"证据A",                      // 当前节点 Result
		"证据B " + pendingResultMark, // 未返回结果的 Result 需标注
		"结论A",                      // 当前节点 Finding
		"被中断的上下文A",                 // 当前节点中断上下文
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
}

// TestBuildContextNoRelated 验证无前序节点时的分区占位。
func TestBuildContextNoRelated(t *testing.T) {
	st := New("id1", "t1", "s1", 1, "首个研究需求")
	ctx := st.BuildContext()
	if !strings.Contains(ctx, "（无前序节点）") {
		t.Fatalf("should mark no related nodes:\n%s", ctx)
	}
	if !strings.Contains(ctx, "# 当前研究节点（第 1 个）") {
		t.Fatalf("should mark current node ordinal:\n%s", ctx)
	}
}

// TestNewWithRelated 验证新节点携带前序节点的完整快照（而非摘要）。
func TestNewWithRelated(t *testing.T) {
	prev := New("id0", "t1", "s1", 1, "上一个研究需求")
	prev.Results = append(prev.Results, Result{Turn: 1, Content: "上一轮证据", Status: ResultCommitted})
	prev.Findings = append(prev.Findings, Finding{Turn: 1, Content: "上一个结论"})
	prev.InterruptedContext = "上一轮中断原文"

	st := NewWithRelated("id1", "t2", "s1", 2, "新研究需求", []RelatedNode{prev.Snapshot()})

	if st.Requirement != "新研究需求" {
		t.Fatalf("requirement = %q", st.Requirement)
	}
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
	// 快照必须完整保留证据、结论与中断原文
	if len(node.Results) != 1 || node.Results[0].Content != "上一轮证据" {
		t.Fatalf("node results = %+v", node.Results)
	}
	if len(node.Findings) != 1 || node.Findings[0].Content != "上一个结论" {
		t.Fatalf("node findings = %+v", node.Findings)
	}
	if node.InterruptedContext != "上一轮中断原文" {
		t.Fatalf("node interrupted context = %q", node.InterruptedContext)
	}
}
