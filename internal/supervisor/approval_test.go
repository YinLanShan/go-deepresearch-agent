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

// TestToolApprovalInterruptResume 验证工具审批走 Eino 的中断/恢复机制：
//  1. 工具执行前中断，并把执行状态写入 Checkpoint；
//  2. 无审批时本轮不提交 Finding；
//  3. 批准后经 Resume 恢复，工具真正执行并提交 Finding。
func TestToolApprovalInterruptResume(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	reg := agenttool.NewRegistry(ctx, agenttool.RegistryConfig{
		EnableBuiltinSearch: true,
		RequireApproval:     []string{"*"},
	})
	if reg.Len() == 0 {
		t.Fatal("no tools registered")
	}

	bus := event.NewBus()
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
	defer shutdownLoop(t, loop)

	// 触发一轮研究：模型会请求 web_search，需要审批。
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "调研执行模型"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 等待进入待审批状态。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !loop.HasPendingApproval() {
		time.Sleep(10 * time.Millisecond)
	}
	if !loop.HasPendingApproval() {
		t.Fatal("loop did not enter pending approval state")
	}

	// 中断期间不应提交 Finding。
	st, _ := states.Get(ctx, "t1")
	if len(st.Findings) != 0 {
		t.Fatalf("finding should not be committed before approval, got %+v", st.Findings)
	}

	// Checkpoint 应已保存（Eino 负责序列化）。cleanup 在 Run 退出时执行，故需轮询。
	kpDeadline := time.Now().Add(5 * time.Second)
	var saved []byte
	for time.Now().Before(kpDeadline) {
		data, existed, err := store.Get(ctx, loop.checkpointID())
		if err != nil {
			t.Fatalf("get checkpoint: %v", err)
		}
		if existed && len(data) > 0 {
			saved = data
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if saved == nil {
		t.Fatalf("checkpoint %q should exist after interrupt", loop.checkpointID())
	}
	t.Logf("checkpoint saved: %d bytes", len(saved))

	// 用同名字段反解，确认是否包含 Runner 状态（决定能否走 GenResume）。
	var decoded struct {
		RunnerCheckpoint []byte
		HasRunnerState   bool
		UnhandledItems   []Input
		CanceledItems    []Input
	}
	if err := gob.NewDecoder(bytes.NewReader(saved)).Decode(&decoded); err != nil {
		t.Logf("decode checkpoint failed: %v", err)
	} else {
		t.Logf("HasRunnerState=%v runnerBytes=%d canceled=%d unhandled=%d",
			decoded.HasRunnerState, len(decoded.RunnerCheckpoint),
			len(decoded.CanceledItems), len(decoded.UnhandledItems))
	}

	// 批准并恢复。
	if err := loop.Approve("", true, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}

	waitFindings(t, states, "t1", 1)

	st, _ = states.Get(ctx, "t1")
	t.Logf("finding = %q", st.Findings[0].Content)
	if st.Findings[0].Content == "" {
		t.Fatal("finding content should not be empty")
	}
}
