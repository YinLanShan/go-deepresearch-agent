package workpool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// fakeTool 构造一个可被列举的假工具。
func fakeTool(t *testing.T, name string) tool.BaseTool {
	t.Helper()
	type fakeInput struct {
		Q string `json:"q"`
	}
	tl, err := utils.InferTool[fakeInput](name, "fake tool "+name,
		func(context.Context, fakeInput) (string, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("build tool %s: %v", name, err)
	}
	return tl
}

// TestFilterSubAgentTools 验证控制类工具被过滤掉，业务工具保留。
func TestFilterSubAgentTools(t *testing.T) {
	all := []tool.BaseTool{
		fakeTool(t, "web_search"),
		fakeTool(t, "submit_assignments"),
		fakeTool(t, "cancel_assignments"),
		fakeTool(t, "finish_research"),
	}

	got := filterSubAgentTools(all)
	if len(got) != 1 {
		t.Fatalf("want only web_search, got %d tools", len(got))
	}
	info, err := got[0].Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "web_search" {
		t.Fatalf("unexpected tool kept: %s", info.Name)
	}
}

// TestSubAgentFactoryCreatesIndependentWorkers 验证每个 Work 拿到独立实例。
//
// 并发安全的关键：子 Agent 内部持有自己的 AsyncIterator，不能共享。
func TestSubAgentFactoryCreatesIndependentWorkers(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{
		Model: newScriptedModel(t, "结论"),
		Tools: []tool.BaseTool{fakeTool(t, "web_search")},
	})

	w1 := &domain.Work{ID: "w1", Goal: "目标一"}
	w2 := &domain.Work{ID: "w2", Goal: "目标二"}

	wk1, err := f.NewWorker(context.Background(), w1)
	if err != nil {
		t.Fatalf("new worker 1: %v", err)
	}
	wk2, err := f.NewWorker(context.Background(), w2)
	if err != nil {
		t.Fatalf("new worker 2: %v", err)
	}
	if wk1 == wk2 {
		t.Fatal("each work must get its own worker instance")
	}
	if wk1.ID() == wk2.ID() {
		t.Fatalf("worker ids should differ, both %q", wk1.ID())
	}
}

// TestSubAgentInstructionUsesGoal 验证提示词按 Work 目标生成。
func TestSubAgentInstructionUsesGoal(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{Model: newScriptedModel(t, "结论")})

	w := &domain.Work{ID: "w1", Goal: "分析 Eino 的执行模型"}
	wk, err := f.NewWorker(context.Background(), w)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	sw, ok := wk.(*SubAgentWorker)
	if !ok {
		t.Fatalf("unexpected worker type %T", wk)
	}
	if !strings.Contains(sw.instruction, "分析 Eino 的执行模型") {
		t.Fatalf("instruction should embed goal:\n%s", sw.instruction)
	}
}

// TestSubAgentWorkerCollectsAssistantOutput 验证多段 assistant 输出被累积为 Result。
//
// ReAct agent 在没有工具调用时会结束本轮，因此这里让模型先发起一次工具调用，
// 再输出最终结论——这样才会产生两段（或多段）assistant 文本。
func TestSubAgentWorkerCollectsAssistantOutput(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{
		Model: newToolThenTextModel("先检索相关资料。", "web_search", "最终结论。"),
		Tools: []tool.BaseTool{fakeTool(t, "web_search")},
	})
	w := &domain.Work{ID: "w1", Goal: "目标"}
	wk, err := f.NewWorker(context.Background(), w)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	var stages []string
	out, err := wk.Run(context.Background(), w, func(stage, detail string) {
		stages = append(stages, stage)
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, "先检索相关资料。") {
		t.Fatalf("result should include pre-tool output, got %q", out)
	}
	if !strings.Contains(out, "最终结论。") {
		t.Fatalf("result should include final output, got %q", out)
	}
	if len(stages) == 0 {
		t.Fatal("progress should be reported at least once")
	}
}

// TestSubAgentWorkerCancelStops 验证取消会立刻终止子 Agent。
func TestSubAgentWorkerCancelStops(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{Model: newBlockingModel()})
	w := &domain.Work{ID: "w1", Goal: "目标"}
	wk, err := f.NewWorker(context.Background(), w)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := wk.Run(ctx, w, func(string, string) {})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}
}

// TestSubAgentWorkerTimeoutStops 验证超时也会终止（由 WorkPool 的 ctx 驱动）。
func TestSubAgentWorkerTimeoutStops(t *testing.T) {
	f := NewSubAgentFactory(SubAgentConfig{Model: newBlockingModel()})
	w := &domain.Work{ID: "w1", Goal: "目标"}
	wk, err := f.NewWorker(context.Background(), w)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := wk.Run(ctx, w, func(string, string) {})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("want deadline/canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop after timeout")
	}
}

// ─── 测试用模型 ──────────────────────────────────────────────────────────────

// scriptedModel 依次输出预设内容，然后结束。
type scriptedModel struct {
	mu     sync.Mutex
	parts  []string
	called int
}

func newScriptedModel(t *testing.T, parts ...string) *scriptedModel {
	t.Helper()
	return &scriptedModel{parts: parts}
}

func (m *scriptedModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.called >= len(m.parts) {
		return schema.AssistantMessage("", nil), nil
	}
	out := m.parts[m.called]
	m.called++
	return schema.AssistantMessage(out, nil), nil
}

func (m *scriptedModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}

// slowScriptedModel 每段输出之间有延迟，用于验证取消/超时。
type slowScriptedModel struct {
	delay time.Duration
	mu    sync.Mutex
	n     int
}

func (m *slowScriptedModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.n++
	n := m.n
	m.mu.Unlock()
	if n > 100 {
		return schema.AssistantMessage("", nil), nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.delay):
	}
	return schema.AssistantMessage("片段", nil), nil
}

func (m *slowScriptedModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}

// blockingModel 一直阻塞到 ctx 结束，用于验证取消/超时确实能终止子 Agent。
type blockingModel struct{}

func newBlockingModel() *blockingModel { return &blockingModel{} }

func (m *blockingModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// toolThenTextModel 首次调用发起一次工具调用，其后输出最终文本。
//
// 用于验证"多段 assistant 输出累积成 Result"：只有发生工具调用，
// ReAct agent 才会继续下一轮，从而产生多段 assistant 文本。
type toolThenTextModel struct {
	preTool  string
	toolName string
	final    string

	mu    sync.Mutex
	calls int
}

func newToolThenTextModel(preTool, toolName, final string) *toolThenTextModel {
	return &toolThenTextModel{preTool: preTool, toolName: toolName, final: final}
}

func (m *toolThenTextModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()

	if n == 1 {
		msg := schema.AssistantMessage(m.preTool, nil)
		msg.ToolCalls = []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      m.toolName,
				Arguments: `{"q":"x"}`,
			},
		}}
		return msg, nil
	}
	return schema.AssistantMessage(m.final, nil), nil
}

func (m *toolThenTextModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}
