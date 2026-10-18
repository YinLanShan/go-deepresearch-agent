// Package supervisor 提供 SupervisorLoop 的最小闭环。
//
// SupervisorLoop 外层是事件单元（订阅用户消息与 ResearchState Commit 事件），
// 内层是 Eino 的 TurnLoop 机制驱动 ChatModelAgent 完成研究决策。
//
// 本阶段实现：
//   - 用户消息经事件进入 TurnLoop，模型流式产出研究结论；
//   - 研究结论（Finding）由"累积的被中断上下文 + 模型回答"构成，并 Commit 到 ResearchState；
//   - ResearchState 出现新 Commit 时，通过事件抢占 SupervisorLoop（在模型/工具调用间隙中断），
//     并把中断前正在产生的上下文原文累积保存到 ResearchState 的 InterruptedContext 字段。
//
// 暂不实现工具、中断、中止、Result 提交、上下文压缩等逻辑。
package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// replanPrompt 是抢占后重新规划轮次使用的固定提示词。
const replanPrompt = "（新的研究结果已提交，请结合最新上下文重新评估并继续研究。）"

// Input 是投递给 SupervisorLoop 的一个输入项。
type Input struct {
	// Message 是用户消息；为抢占触发时可携带固定 replanPrompt。
	Message string
}

// Config 是 SupervisorLoop 的配置。
type Config struct {
	Model     model.BaseChatModel
	States    researchstate.Store
	SessionID string
	Bus       *event.Bus
}

// Loop 是 SupervisorLoop 的最小闭环。
type Loop struct {
	sessionID string
	model     model.BaseChatModel
	states    researchstate.Store
	bus       *event.Bus

	loop *adk.TurnLoop[Input, *schema.Message]
	once sync.Once
}

// 编译期断言实现事件订阅接口。
var _ event.EventSender = (*Loop)(nil)

// New 构造一个 SupervisorLoop 并订阅事件总线。
func New(cfg Config) *Loop {
	l := &Loop{
		sessionID: cfg.SessionID,
		model:     cfg.Model,
		states:    cfg.States,
		bus:       cfg.Bus,
	}
	l.loop = adk.NewTurnLoop(adk.TurnLoopConfig[Input, *schema.Message]{
		GenInput:      l.genInput,
		PrepareAgent:  l.prepareAgent,
		OnAgentEvents: l.onAgentEvents,
	})
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventUserMessage, l)
		cfg.Bus.Subscribe(event.EventResearchStateCommitted, l)
	}
	return l
}

// Start 启动 TurnLoop（非阻塞）。
func (l *Loop) Start() {
	l.loop.Run(context.Background())
}

// Send 实现 event.EventSender：过滤本 Session 的事件并推入 TurnLoop。
func (l *Loop) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.UserMessageEvent:
		if ev.SessionID != l.sessionID {
			return nil
		}
		l.loop.Push(Input{Message: ev.Message})
	case event.ResearchStateCommittedEvent:
		if ev.SessionID != l.sessionID {
			return nil
		}
		// 抢占当前轮次（在模型/工具调用间隙中断），并开启重新规划轮次。
		l.loop.Push(Input{Message: replanPrompt},
			adk.WithPreempt[Input, *schema.Message](adk.AnySafePoint))
	}
	return nil
}

// Shutdown 停止 TurnLoop 并等待退出。
func (l *Loop) Shutdown(ctx context.Context) error {
	l.once.Do(func() { l.loop.Stop() })
	done := make(chan struct{})
	go func() {
		l.loop.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// genInput 把缓冲的用户消息组织成本轮 Agent 输入（启用流式，便于中断时捕获原文）。
func (l *Loop) genInput(_ context.Context, _ *adk.TurnLoop[Input, *schema.Message],
	items []Input) (*adk.GenInputResult[Input, *schema.Message], error) {
	msgs := make([]*schema.Message, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Message) == "" {
			msgs = append(msgs, schema.UserMessage(replanPrompt))
		} else {
			msgs = append(msgs, schema.UserMessage(it.Message))
		}
	}
	return &adk.GenInputResult[Input, *schema.Message]{
		Input:    &adk.AgentInput{Messages: msgs, EnableStreaming: true},
		Consumed: items,
	}, nil
}

// prepareAgent 每轮根据最新 ResearchState 构建系统提示词并创建 Agent。
func (l *Loop) prepareAgent(ctx context.Context, _ *adk.TurnLoop[Input, *schema.Message],
	_ []Input) (adk.Agent, error) {
	instruction := ""
	if st, err := l.states.Get(ctx, l.sessionID); err == nil {
		instruction = st.BuildContext()
	}
	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "supervisor",
		Description: "research supervisor",
		Instruction: instruction,
		Model:       l.model,
	})
}

// onAgentEvents 消费 Agent 事件（流式），累积模型输出；
// 若本轮被抢占，则把已产生的上下文原文保存为"被中断的上下文"；
// 否则把"被中断上下文 + 模型回答"作为 Finding Commit 到 ResearchState。
func (l *Loop) onAgentEvents(ctx context.Context, tc *adk.TurnContext[Input, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent]) error {
	var output strings.Builder

	for {
		event, ok := events.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			continue // 忽略错误事件（如抢占产生的 CancelError）
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mv := event.Output.MessageOutput
		if mv.Role != schema.Assistant {
			continue
		}
		if mv.IsStreaming {
			if err := l.consumeStream(&output, mv.MessageStream); err != nil {
				break
			}
		} else if mv.Message != nil && mv.Message.Content != "" {
			output.WriteString(mv.Message.Content)
		}

		if preempted(tc) {
			l.saveInterrupted(ctx, output.String())
			return nil
		}
	}

	if preempted(tc) {
		l.saveInterrupted(ctx, output.String())
		return nil // 不发布 done：等待重新规划轮次继续产出
	}

	err := l.commitFinding(ctx, output.String())
	l.publishOutputDone()
	return err
}

// consumeStream 逐块读取流式输出，累积到 output 并发布 ModelOutput 事件；
// 流被取消（抢占）时停止。
func (l *Loop) consumeStream(output *strings.Builder, sr *schema.StreamReader[*schema.Message]) error {
	if sr == nil {
		return nil
	}
	defer sr.Close()
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err // 流被取消（抢占）或出错
		}
		if chunk != nil && chunk.Content != "" {
			output.WriteString(chunk.Content)
			l.publishOutput(chunk.Content)
		}
	}
}

// publishOutput 发布一块模型流式输出。
func (l *Loop) publishOutput(chunk string) {
	if l.bus == nil || chunk == "" {
		return
	}
	_ = l.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.ModelOutputEvent{SessionID: l.sessionID, Chunk: chunk},
	})
}

// publishOutputDone 发布流式输出结束事件。
func (l *Loop) publishOutputDone() {
	if l.bus == nil {
		return
	}
	_ = l.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.ModelOutputDoneEvent{SessionID: l.sessionID},
	})
}

// commitFinding 把"被中断上下文 + 模型回答"作为 Finding Commit 到 ResearchState。
func (l *Loop) commitFinding(ctx context.Context, answer string) error {
	if answer == "" {
		return nil
	}
	st, err := l.states.Get(ctx, l.sessionID)
	if err != nil {
		return err
	}
	finding := st.InterruptedContext + answer
	return l.states.Commit(ctx, l.sessionID, researchstate.CommitInput{
		ResultContent:  answer,
		FindingContent: finding,
	})
}

// saveInterrupted 把中断前正在产生的上下文原文累积保存到 ResearchState。
func (l *Loop) saveInterrupted(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if err := l.states.AccumulateInterruptedContext(ctx, l.sessionID, text); err != nil {
		slog.Error("accumulate interrupted context failed", "session", l.sessionID, "err", err)
	}
}

// preempted 非阻塞判断当前轮次是否被抢占。
func preempted(tc *adk.TurnContext[Input, *schema.Message]) bool {
	select {
	case <-tc.Preempted:
		return true
	default:
		return false
	}
}
