package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/checkpoint"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
)

// finishRecorder 记录 ResearchFinished 事件。
type finishRecorder struct {
	mu       sync.Mutex
	finished []event.ResearchFinishedEvent
}

var _ event.EventSender = (*finishRecorder)(nil)

func (r *finishRecorder) Send(_ context.Context, e event.EventEnvelope) error {
	if ev, ok := e.Data.(event.ResearchFinishedEvent); ok {
		r.mu.Lock()
		r.finished = append(r.finished, ev)
		r.mu.Unlock()
	}
	return nil
}

func (r *finishRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.finished)
}

// TestFinishResearchEndsLoop 验证模型调用 finish_research 后：
//   - 发布 ResearchFinished（研究终结）；
//   - Click 后不再接受新的推理输入（不许"反悔"）。
func TestFinishResearchEndsLoop(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "")); err != nil {
		t.Fatalf("create state: %v", err)
	}

	// 模型第一步就调用 finish_research。
	model := mockModel.NewToolModel()
	model.ToolName = "finish_research"
	model.Arguments = `{"conclusion":"研究目标已达成"}`

	bus := event.NewBus()
	rec := &finishRecorder{}
	bus.Subscribe(event.EventResearchFinished, rec)

	loop := New(Config{
		Model:       model,
		States:      states,
		SessionID:   "s1",
		TaskID:      "t1",
		RunTaskID:   "r1",
		Bus:         bus,
		Tools:       []tool.BaseTool{},
		Policy:      agenttool.Policy{},
		CheckPoints: checkpoint.NewMemoryStore(),
	})
	loop.Start()
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "调研并结束"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 应发布一次 ResearchFinished。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && rec.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.count() != 1 {
		t.Fatalf("want exactly 1 research_finished event, got %d", rec.count())
	}

	// 终结 latch 应置位。
	if !loop.finishing {
		t.Fatal("loop should be in finishing state")
	}

	// 终结后再推消息应被忽略：不应产生新的 Finding。
	st, _ := states.Get(ctx, "t1")
	before := len(st.Findings)
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "再想想"}}); err != nil {
		t.Fatalf("publish after finish: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	st, _ = states.Get(ctx, "t1")
	if len(st.Findings) != before {
		t.Fatalf("no new finding should be committed after finish: before=%d after=%d",
			before, len(st.Findings))
	}
	if rec.count() != 1 {
		t.Fatalf("research_finished should not be published again, got %d", rec.count())
	}
}

// TestTurnEndWithoutFinishKeepsRunning 验证"本轮结束"不等于"研究终结"：
// 模型不调用 finish_research 时只发布输出结束事件，不发布 ResearchFinished。
func TestTurnEndWithoutFinishKeepsRunning(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1, "")); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	rec := &finishRecorder{}
	bus.Subscribe(event.EventResearchFinished, rec)

	loop := New(Config{
		Model:       mockModel.New(),
		States:      states,
		SessionID:   "s1",
		TaskID:      "t1",
		RunTaskID:   "r1",
		Bus:         bus,
		Tools:       []tool.BaseTool{},
		Policy:      agenttool.Policy{},
		CheckPoints: checkpoint.NewMemoryStore(),
	})
	loop.Start()
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "随便聊聊"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 本轮会提交 Finding，但不应宣告研究结束。
	waitFindings(t, states, "t1", 1)
	time.Sleep(300 * time.Millisecond)

	if rec.count() != 0 {
		t.Fatalf("turn end must not publish research_finished, got %d", rec.count())
	}
	if loop.finishing {
		t.Fatal("loop should not be finishing without finish_research")
	}
}
