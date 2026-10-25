package workpool

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// TestSubAgentNeverRequestsApproval 验证子 Agent 不参与人工审批。
//
// 即使工具在 Supervisor 侧被配置为"需要审批"，子 Agent 也不注入审批中间件，
// 因此不会产生审批中断；工具会被直接执行。
func TestSubAgentNeverRequestsApproval(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{
		Model: newToolThenTextModel("先调用工具。", "sensitive_tool", "最终结论。"),
		Tools: []tool.BaseTool{fakeTool(t, "sensitive_tool")},
	})
	w := &domain.Work{ID: "w1", Goal: "目标"}
	wk, err := f.NewWorker(context.Background(), w)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	out, err := wk.Run(context.Background(), w, func(string, string) {})
	if err != nil {
		t.Fatalf("sub-agent should not interrupt for approval, got %v", err)
	}
	if out == "" {
		t.Fatal("sub-agent should produce output")
	}
}

// TestTimeoutDoesNotSubmitResult 验证超时的 Work 不提交结果。
//
// 超时直接终止：Work 标记为 timedout，且不发布 WorkResultReady 事件，
// 因此不会有 Result 进入 ResearchState。
func TestTimeoutDoesNotSubmitResult(t *testing.T) {
	works := memory.NewWorkStore()
	bus := event.NewBus()

	// 记录所有结果为就绪的事件。
	var ready int
	rec := &resultReadyRecorder{onReady: func() { ready++ }}
	bus.Subscribe(event.EventWorkResultReady, rec)

	factory := &countingFactory{delay: 2 * time.Second} // 远大于超时
	p := New(Config{
		Works: works, Bus: bus, Factory: factory, Workers: 1,
		WorkTimeout: 80 * time.Millisecond,
	})
	p.Start()
	defer func() { _ = p.Shutdown(context.Background()) }()

	w := &domain.Work{
		ID: "w1", AssignmentID: "a1", SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
		Goal: "目标", Status: domain.WorkPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := works.Create(context.Background(), w); err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := p.Submit(context.Background(), w); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if !waitForTest(3*time.Second, func() bool {
		got, err := works.Get(context.Background(), "w1")
		return err == nil && got.Status == domain.WorkTimedOut
	}) {
		got, _ := works.Get(context.Background(), "w1")
		t.Fatalf("work should be timedout, got %+v", got)
	}

	// 再等一会儿，确认没有结果事件漏出来
	time.Sleep(150 * time.Millisecond)
	if ready != 0 {
		t.Fatalf("timed out work must not submit results, got %d result events", ready)
	}

	got, _ := works.Get(context.Background(), "w1")
	if got.Result != "" {
		t.Fatalf("timed out work should have no result, got %q", got.Result)
	}
}

// resultReadyRecorder 记录 WorkResultReady 事件次数。
type resultReadyRecorder struct {
	onReady func()
}

func (r *resultReadyRecorder) Send(_ context.Context, e event.EventEnvelope) error {
	if _, ok := e.Data.(event.WorkResultReadyEvent); ok && r.onReady != nil {
		r.onReady()
	}
	return nil
}
