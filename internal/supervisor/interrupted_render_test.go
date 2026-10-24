package supervisor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
)

// instructionRecorder 记录每次模型调用时收到的 system 提示词。
type instructionRecorder struct {
	inner model.BaseChatModel
	mu    sync.Mutex
	texts []string
}

func (r *instructionRecorder) record(in []*schema.Message) {
	var sb strings.Builder
	for _, m := range in {
		if m != nil && m.Role == schema.System {
			sb.WriteString(m.Content)
		}
	}
	r.mu.Lock()
	r.texts = append(r.texts, sb.String())
	r.mu.Unlock()
}

func (r *instructionRecorder) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	r.record(in)
	return r.inner.Generate(ctx, in, opts...)
}

func (r *instructionRecorder) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.record(in)
	return r.inner.Stream(ctx, in, opts...)
}

// sawInterrupted 判断是否有某次调用的提示词渲染了被中断的原文。
func (r *instructionRecorder) sawInterrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.texts {
		if strings.Contains(t, "上一轮被中断的输出") && strings.Contains(t, "mock reply to:") {
			return true
		}
	}
	return false
}

func (r *instructionRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.texts)
}

func (r *instructionRecorder) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.texts, "\n---\n")
}

// TestReentryRendersInterruptedContext 验证核心诉求：
// 被打断后重入时，模型收到的提示词里必须包含中断前的原文。
//
// 若只把原文存进 InterruptedContext 而不渲染，模型看不到自己中断前写到哪，
// 等于丢失了实际上下文。
func TestReentryRendersInterruptedContext(t *testing.T) {
	ctx := context.Background()
	states := stateMemory.NewStore()
	if err := states.Create(ctx, researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}

	bus := event.NewBus()
	rec := &instructionRecorder{inner: mockModel.NewSlowModel(30 * time.Millisecond)}
	loop := New(Config{
		Model:     rec,
		States:    states,
		SessionID: "s1",
		TaskID:    "t1",
		RunTaskID: "r1",
		Bus:       bus,
	})
	loop.Start()
	defer shutdownLoop(t, loop)

	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.UserMessageEvent{
		SessionID: "s1", TaskID: "t1", RunTaskID: "r1", Message: "请分析架构"}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 等模型输出部分 chunk 后触发抢占
	time.Sleep(80 * time.Millisecond)
	if err := bus.Publish(ctx, event.EventEnvelope{Data: event.ResearchStateCommittedEvent{
		SessionID: "s1", TaskID: "t1", Version: 1}}); err != nil {
		t.Fatalf("publish preempt: %v", err)
	}

	waitFindings(t, states, "t1", 1)

	if rec.count() < 2 {
		t.Fatalf("expect >=2 model calls (interrupted + re-entry), got %d", rec.count())
	}
	if !rec.sawInterrupted() {
		t.Fatalf("re-entry instruction must render interrupted output; calls:\n%s", rec.dump())
	}
}
