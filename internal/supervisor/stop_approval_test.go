package supervisor

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/checkpoint"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
)

// checkpointMeta 对应 TurnLoop checkpoint 的可导出字段（与 Eino 内部字段同名，可 gob 反解）。
type checkpointMeta struct {
	RunnerCheckpoint []byte
	HasRunnerState   bool
	UnhandledItems   []Input
	CanceledItems    []Input
}

func readCheckpoint(t *testing.T, store *checkpoint.MemoryStore, id string) (checkpointMeta, bool) {
	t.Helper()
	var meta checkpointMeta
	data, existed, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if !existed || len(data) == 0 {
		return meta, false
	}
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&meta); err != nil {
		t.Logf("decode checkpoint failed: %v", err)
	}
	return meta, true
}

// approvalRecorder 记录审批请求与作废事件，便于断言。
type approvalRecorder struct {
	required  []event.ToolApprovalRequiredEvent
	cancelled []event.ToolApprovalCancelledEvent
}

var _ event.EventSender = (*approvalRecorder)(nil)

func (r *approvalRecorder) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.ToolApprovalRequiredEvent:
		r.required = append(r.required, ev)
	case event.ToolApprovalCancelledEvent:
		r.cancelled = append(r.cancelled, ev)
	}
	return nil
}

// TestStopWhilePendingApproval 验证"中止是终态"的语义：
//   - 待审批项被作废，并对外发布 approval_cancelled；
//   - Checkpoint 被清理，任务不可恢复；
//   - Approve 被拒绝，不会执行工具；
//   - ResearchState（含 InterruptedContext）作为历史节点保留。
func TestStopWhilePendingApproval(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	reg := agenttool.NewRegistry(ctx, agenttool.RegistryConfig{
		EnableBuiltinSearch: true,
		RequireApproval:     []string{"*"},
	})

	bus := event.NewBus()
	rec := &approvalRecorder{}
	bus.Subscribe(event.EventToolApprovalRequired, rec)
	bus.Subscribe(event.EventToolApprovalCancelled, rec)

	store := checkpoint.NewMemoryStore()
	loop := New(Config{
		Model:       mockModel.NewToolModel(),
		States:      states,
		SessionID:   "s1",
		TaskID:      "t1",
		RunTaskID:   "r1",
		Bus:         bus,
		Tools:       reg.Tools(),
		Policy:      reg.Policy(),
		CheckPoints: store,
	})
	loop.Start()

	// 进入待审批状态。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "调研执行模型"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !loop.HasPendingApproval() {
		time.Sleep(10 * time.Millisecond)
	}
	if !loop.HasPendingApproval() {
		t.Fatal("did not enter pending approval")
	}

	// 等 Checkpoint 落盘（含 runner 状态）。
	for time.Now().Before(deadline) {
		if meta, ok := readCheckpoint(t, store, loop.CheckpointID()); ok && meta.HasRunnerState {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, ok := readCheckpoint(t, store, loop.CheckpointID())
	if !ok || !before.HasRunnerState {
		t.Fatalf("checkpoint should hold runner state before stop, got %+v", before)
	}

	// 中止：作废审批 + 清理 Checkpoint。
	cancelledID := loop.CancelPendingApproval("任务已中止")
	if cancelledID == "" {
		t.Fatal("should return cancelled approval id")
	}
	if err := store.Delete(ctx, loop.CheckpointID()); err != nil {
		t.Fatalf("delete checkpoint: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.Shutdown(sctx); err != nil {
		t.Logf("shutdown: %v", err)
	}

	// 1) 审批项已作废且对外发布。
	if loop.HasPendingApproval() {
		t.Fatal("pending approval should be cleared after stop")
	}
	if len(rec.cancelled) != 1 || rec.cancelled[0].ApprovalID != cancelledID {
		t.Fatalf("expected one approval_cancelled event for %q, got %+v", cancelledID, rec.cancelled)
	}

	// 2) Checkpoint 已清理，不可恢复。
	if _, existed := readCheckpoint(t, store, loop.CheckpointID()); existed {
		t.Fatal("checkpoint should be deleted after stop")
	}

	// 3) 中止后不能再批准。
	if err := loop.Approve("", true, ""); err == nil {
		t.Fatal("approve after stop should fail")
	}

	// 4) 未执行工具、未提交 Finding；ResearchState 仍保留历史。
	st, err := states.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if len(st.Findings) != 0 {
		t.Fatalf("no finding should be committed, got %+v", st.Findings)
	}
	if len(st.History) == 0 {
		t.Fatal("history should be preserved as the node's record")
	}
}
