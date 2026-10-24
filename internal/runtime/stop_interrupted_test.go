package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// TestStoppedNodeCarriesInterruptedContext 验证中止节点的残留原文会被下一个 Task 继承。
//
// 中止（stopped）不会 Commit，因此 InterruptedContext 会残留；
// 通过 RelatedNode 快照带出后，后续节点的上下文里应能看到这段未完成的输出。
func TestStoppedNodeCarriesInterruptedContext(t *testing.T) {
	h := newExecutionRuntime(t)
	ctx := context.Background()

	// 第一个任务：启用后直接中止，并注入一段"被打断"的原文。
	if err := h.tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "第一个"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	enable(t, h.bus, "t1")
	waitStatus(t, h.runs, "t1", domain.RunStatusRunning)

	const partial = "写到一半的分析：框架 A 的执行模型是……"
	if err := h.states.AccumulateInterruptedContext(ctx, "t1", partial); err != nil {
		t.Fatalf("accumulate interrupted: %v", err)
	}

	// 用户中止该任务。
	if err := h.bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.CoordinationRequestedEvent{
			Action: event.ActionStop, TaskID: "t1",
		},
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	prev, err := h.states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if strings.TrimSpace(prev.InterruptedContext) == "" {
		t.Fatal("stopped node should retain interrupted context (no commit happened)")
	}

	// 第二个任务：其 RelatedNode 应携带上一节点的残留原文。
	if err := h.tasks.Create(ctx, &domain.Task{ID: "t2", SessionID: "s1", Title: "第二个"}); err != nil {
		t.Fatalf("create task2: %v", err)
	}
	enable(t, h.bus, "t2")
	waitStatus(t, h.runs, "t2", domain.RunStatusRunning)

	cur, err := h.states.Get(ctx, "t2")
	if err != nil {
		t.Fatalf("get state2: %v", err)
	}
	if len(cur.RelatedNodes) == 0 {
		t.Fatal("new node should inherit related nodes")
	}

	var found *researchstate.RelatedNode
	for i := range cur.RelatedNodes {
		if cur.RelatedNodes[i].TaskID == "t1" {
			found = &cur.RelatedNodes[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("related nodes should include t1, got %+v", cur.RelatedNodes)
	}
	if !strings.Contains(found.InterruptedContext, "写到一半的分析") {
		t.Fatalf("related node should carry interrupted text, got %q", found.InterruptedContext)
	}

	// 且该原文会渲染进新节点的上下文。
	built := cur.BuildContext()
	if !strings.Contains(built, "该节点中止时未完成的输出") {
		t.Fatalf("context should render the stopped node's interrupted section:\n%s", built)
	}
	if !strings.Contains(built, partial) {
		t.Fatalf("context should render the interrupted text:\n%s", built)
	}
}

// TestFinishedNodeHasNoInterruptedContext 验证正常结束的节点不残留该字段。
func TestFinishedNodeHasNoInterruptedContext(t *testing.T) {
	st := researchstate.New("id1", "t1", "s1", 1)
	st.Findings = append(st.Findings, researchstate.Finding{Turn: 1, Content: "已完成的过程"})
	st.Conclusion = "最终结论"

	node := st.Snapshot()
	if node.InterruptedContext != "" {
		t.Fatalf("finished node should not carry interrupted context, got %q", node.InterruptedContext)
	}

	cur := researchstate.NewWithRelated("id2", "t2", "s1", 2, []researchstate.RelatedNode{node})
	if strings.Contains(cur.BuildContext(), "该节点中止时未完成的输出") {
		t.Fatal("finished node should not render interrupted section")
	}
}
