package researchstate

import (
	"strings"
	"testing"
)

func TestBuildContext(t *testing.T) {
	st := New("id1", "t1", "s1", "研究某框架的架构与性能")

	ctx := st.BuildContext()
	for _, want := range []string{
		"研究某框架的架构与性能", // 研究需求
		"研究产物（Result / 证据）",
		"研究结论（Finding）",
		"被中断的上下文原文",
		"关联的先前研究结论",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}

	// 追加研究产物（含未返回结果的 Result）与结论
	st.Results = append(st.Results,
		Result{Turn: 1, Content: "证据A", Status: ResultCommitted},
		Result{Turn: 2, Content: "证据B", Status: ResultPending},
	)
	st.Findings = append(st.Findings, Finding{Turn: 1, Content: "结论A"})
	st.InterruptedContext = "被中断的上下文A"
	st.RelatedFindings = append(st.RelatedFindings, RelatedFinding{SessionID: "s0", Summary: "先前结论"})

	ctx = st.BuildContext()
	for _, want := range []string{
		"证据A",
		"证据B " + pendingResultMark, // 未返回结果的 Result 用固定提示词标注
		"结论A",
		"被中断的上下文A",
		"先前结论",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "（该 Result 尚未返回结果）") == false {
		t.Fatalf("pending result should be marked:\n%s", ctx)
	}
}

func TestNewWithRelated(t *testing.T) {
	prev := New("id0", "t0", "s0", "上一个研究需求")
	prev.Findings = append(prev.Findings, Finding{Turn: 1, Content: "上一个结论"})

	// 新研究任务：新建 ResearchState，携带上一研究任务结论作为 RelatedFinding
	related := []RelatedFinding{{SessionID: "s0", Summary: prev.Summary()}}
	st := NewWithRelated("id1", "t1", "s1", "新研究需求", related)

	if st.Requirement != "新研究需求" {
		t.Fatalf("requirement = %q", st.Requirement)
	}
	if st.TaskID != "t1" {
		t.Fatalf("taskID = %q", st.TaskID)
	}
	if len(st.RelatedFindings) != 1 || !strings.Contains(st.RelatedFindings[0].Summary, "上一个结论") {
		t.Fatalf("related = %+v", st.RelatedFindings)
	}
}
