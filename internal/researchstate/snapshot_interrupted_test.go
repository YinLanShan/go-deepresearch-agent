package researchstate

import (
	"strings"
	"testing"
)

// TestSnapshotCarriesInterruptedContext 验证中止节点的残留原文随快照传给后续节点。
//
// 正常结束的节点在 Commit 时已把原文并入 Finding 并清空该字段；
// 但中止（stopped）的节点不会 Commit，若不带出这段原文，后续节点就完全丢失了。
func TestSnapshotCarriesInterruptedContext(t *testing.T) {
	st := New("id1", "t1", "s1", 1)
	st.InterruptedContext = "写到一半的分析：框架 A 的执行模型是……"

	node := st.Snapshot()
	if node.InterruptedContext != "写到一半的分析：框架 A 的执行模型是……" {
		t.Fatalf("snapshot should carry interrupted context, got %q", node.InterruptedContext)
	}
}

// TestRelatedNodeRendersInterruptedContext 验证前序节点渲染残留原文。
func TestRelatedNodeRendersInterruptedContext(t *testing.T) {
	prev := New("id0", "t1", "s1", 1)
	prev.InterruptedContext = "中止时未写完的段落"

	cur := NewWithRelated("id1", "t2", "s1", 2, []RelatedNode{prev.Snapshot()})
	ctx := cur.BuildContext()

	if !strings.Contains(ctx, "该节点中止时未完成的输出") {
		t.Fatalf("related node should render interrupted section:\n%s", ctx)
	}
	if !strings.Contains(ctx, "中止时未写完的段落") {
		t.Fatalf("related node should render interrupted text:\n%s", ctx)
	}
}

// TestRelatedNodeWithoutInterruptedContext 验证正常结束的节点不渲染该区块。
func TestRelatedNodeWithoutInterruptedContext(t *testing.T) {
	prev := New("id0", "t1", "s1", 1)
	prev.Conclusion = "上一个结论"
	// 正常结束：InterruptedContext 已清空

	cur := NewWithRelated("id1", "t2", "s1", 2, []RelatedNode{prev.Snapshot()})
	ctx := cur.BuildContext()

	if strings.Contains(ctx, "该节点中止时未完成的输出") {
		t.Fatalf("finished node should not render interrupted section:\n%s", ctx)
	}
	if !strings.Contains(ctx, "上一个结论") {
		t.Fatalf("related node should still render its conclusion:\n%s", ctx)
	}
}

// TestSnapshotCopiesSlices 验证快照不与原状态共享切片。
func TestSnapshotCopiesSlices(t *testing.T) {
	st := New("id1", "t1", "s1", 1)
	st.Requirements = []string{"需求 A"}
	st.Findings = append(st.Findings, Finding{Turn: 1, Content: "过程 A"})

	node := st.Snapshot()
	st.Requirements[0] = "被篡改"
	node.Findings[0].Content = "被篡改"

	if st.Requirements[0] != "被篡改" {
		t.Fatal("test setup error")
	}
	// 快照里的切片在生成时就已复制，修改原状态不应影响它
	if node.Requirements[0] != "需求 A" {
		t.Fatalf("snapshot should copy requirements, got %q", node.Requirements[0])
	}
}
