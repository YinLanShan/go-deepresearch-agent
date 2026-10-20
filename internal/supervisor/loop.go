// Package supervisor 提供 SupervisorLoop。
//
// SupervisorLoop 外层是事件单元（订阅用户消息与 ResearchState Commit 事件），
// 内层是 Eino 的 TurnLoop + ChatModelAgent 完成研究决策。
//
// 工具治理完全复用 Eino 机制，不在项目内自行实现：
//   - 工具审批：中间件里调用 tool.StatefulInterrupt 暂停本轮，由 ADK 序列化 Checkpoint；
//     用户决定后通过 ResumeParams 恢复，TurnLoop 的 GenResume 据此重建 ResumeParams。
//   - 工具报错：中间件把工具错误转成文本结果回传模型，中断重入错误原样抛出。
//   - 工具展示：中间件在调用前后上报事件，经事件总线以 SSE 推送到前端。
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
)

// replanPrompt 是抢占后重新规划轮次使用的固定提示词。
const replanPrompt = "（新的研究结果已提交，请结合最新上下文重新评估并继续研究。）"

// ErrNoPendingApproval 表示当前没有等待审批的工具调用。
var ErrNoPendingApproval = errors.New("supervisor: no pending tool approval")

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
	TaskID    string
	RunTaskID string
	Bus       *event.Bus

	// Tools 是注入给 ReAct Agent 的工具（MCP / 内置）。
	Tools []tool.BaseTool
	// Policy 决定哪些工具需要人工审批。
	Policy agenttool.Policy
	// CheckPoints 用于中断时序列化 Checkpoint；为 nil 时不支持审批恢复。
	CheckPoints adk.CheckPointStore
}

// Loop 是 SupervisorLoop。
type Loop struct {
	sessionID string
	taskID    string
	runTaskID string
	model     model.BaseChatModel
	states    researchstate.Store
	bus       *event.Bus

	tools       []tool.BaseTool
	policy      agenttool.Policy
	checkPoints adk.CheckPointStore

	mu sync.Mutex
	// loop 是当前 TurnLoop 实例。审批恢复需要新的实例（Eino 的 Run 只能生效一次）。
	loop *adk.TurnLoop[Input, *schema.Message]
	// pendingInterruptID 是当前等待审批的中断点 ID（空表示无）。
	pendingInterruptID string
	// resumeData 保存用户决定，供 GenResume 构造 ResumeParams。
	resumeData map[string]agenttool.ApprovalResult

	stopOnce sync.Once
	stopped  bool
}

// 编译期断言：实现事件订阅与工具动作上报接口。
var (
	_ event.EventSender  = (*Loop)(nil)
	_ agenttool.Recorder = (*Loop)(nil)
)

// checkpointID 返回该 Task 的 checkpoint 键。
func (l *Loop) checkpointID() string { return "task:" + l.taskID }

// New 构造一个 SupervisorLoop 并订阅事件总线。
func New(cfg Config) *Loop {
	l := &Loop{
		sessionID:   cfg.SessionID,
		taskID:      cfg.TaskID,
		runTaskID:   cfg.RunTaskID,
		model:       cfg.Model,
		states:      cfg.States,
		bus:         cfg.Bus,
		tools:       cfg.Tools,
		policy:      cfg.Policy,
		checkPoints: cfg.CheckPoints,
		resumeData:  make(map[string]agenttool.ApprovalResult),
	}
	l.loop = l.newTurnLoop()
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventUserMessage, l)
		cfg.Bus.Subscribe(event.EventResearchStateCommitted, l)
	}
	return l
}

// newTurnLoop 创建一个 TurnLoop 实例。
//
// 配置 Store + CheckpointID 后，TurnLoop 会在中断时保存 Checkpoint，
// 并在下一次 Run 时通过 GenResume 自动走恢复流程。
func (l *Loop) newTurnLoop() *adk.TurnLoop[Input, *schema.Message] {
	return adk.NewTurnLoop(adk.TurnLoopConfig[Input, *schema.Message]{
		GenInput:      l.genInput,
		GenResume:     l.genResume,
		PrepareAgent:  l.prepareAgent,
		OnAgentEvents: l.onAgentEvents,
		Store:         l.checkPoints,
		CheckpointID:  l.checkpointID(),
	})
}

// Start 启动 TurnLoop（非阻塞）。
func (l *Loop) Start() {
	l.mu.Lock()
	loop := l.loop
	l.mu.Unlock()
	loop.Run(context.Background())
}

// SessionID 返回该 SupervisorLoop 所属的 Session。
func (l *Loop) SessionID() string { return l.sessionID }

// HasPendingApproval 返回是否存在等待审批的中断。
func (l *Loop) HasPendingApproval() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pendingInterruptID != ""
}

// CancelPendingApproval 作废当前待审批项，并返回被作废的审批 ID（无则返回空串）。
//
// 用于"中止"这类终态：审批卡片必须失效，用户不应再能批准一个已经结束的任务。
// 不清理 Checkpoint —— 由调用方按语义决定是否删除（中止会删除）。
func (l *Loop) CancelPendingApproval(reason string) string {
	l.mu.Lock()
	id := l.pendingInterruptID
	l.pendingInterruptID = ""
	l.mu.Unlock()

	if id == "" {
		return ""
	}
	l.publishEvent(event.ToolApprovalCancelledEvent{
		SessionID:  l.sessionID,
		TaskID:     l.taskID,
		RunTaskID:  l.runTaskID,
		ApprovalID: id,
		Reason:     reason,
	})
	return id
}

// CheckpointID 返回该 Task 的 Checkpoint 键，供调用方在终态时清理。
func (l *Loop) CheckpointID() string { return l.checkpointID() }

// Approve 提交一次审批决定，并触发 Eino 的恢复流程。
//
// 恢复方式：用新的 TurnLoop 实例（同一 CheckpointID）重新 Run，
// TurnLoop 读取 Checkpoint 后调用 GenResume；我们不实现恢复本身。
func (l *Loop) Approve(interruptID string, approved bool, reason string) error {
	l.mu.Lock()
	if l.pendingInterruptID == "" {
		l.mu.Unlock()
		return ErrNoPendingApproval
	}
	if interruptID != "" && interruptID != l.pendingInterruptID {
		l.mu.Unlock()
		return fmt.Errorf("%w: interrupt id mismatch", ErrNoPendingApproval)
	}
	if l.stopped {
		l.mu.Unlock()
		return errors.New("supervisor: loop stopped")
	}
	id := l.pendingInterruptID
	l.pendingInterruptID = ""
	l.resumeData[id] = agenttool.ApprovalResult{Approved: approved, Reason: reason}

	loop := l.newTurnLoop()
	l.loop = loop
	l.mu.Unlock()

	loop.Run(context.Background())
	return nil
}

// Shutdown 停止 TurnLoop 并等待退出。
func (l *Loop) Shutdown(ctx context.Context) error {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		l.stopped = true
		loop := l.loop
		l.mu.Unlock()
		loop.Stop()
	})

	done := make(chan struct{})
	go func() {
		l.mu.Lock()
		loop := l.loop
		l.mu.Unlock()
		loop.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send 实现 event.EventSender：按 RunTaskID 过滤消息、按 TaskID 过滤 Commit 事件。
func (l *Loop) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.UserMessageEvent:
		if ev.RunTaskID != l.runTaskID {
			return nil
		}
		// 记录用户消息到对话历史；首条消息同时作为本次研究任务的需求。
		l.recordUserMessage(ev.Message)
		l.push(Input{Message: ev.Message})
	case event.ResearchStateCommittedEvent:
		if ev.TaskID != l.taskID {
			return nil
		}
		// 抢占当前轮次（在模型/工具调用间隙中断），并开启重新规划轮次。
		l.push(Input{Message: replanPrompt}, adk.WithPreempt[Input, *schema.Message](adk.AnySafePoint))
	}
	return nil
}

// push 向当前 TurnLoop 投递输入。
func (l *Loop) push(item Input, opts ...adk.PushOption[Input, *schema.Message]) {
	l.mu.Lock()
	loop := l.loop
	l.mu.Unlock()
	loop.Push(item, opts...)
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

// genResume 由 Eino 在检测到中断 Checkpoint 后调用，用于构造恢复参数。
//
// 这里只负责把"用户决定"放进 ResumeParams；真正的恢复执行由 ADK 完成。
func (l *Loop) genResume(_ context.Context, _ *adk.TurnLoop[Input, *schema.Message],
	interruptedItems, _, _ []Input) (*adk.GenResumeResult[Input, *schema.Message], error) {
	l.mu.Lock()
	var params *adk.ResumeParams
	for id, decision := range l.resumeData {
		params = &adk.ResumeParams{Targets: map[string]any{id: decision}}
		delete(l.resumeData, id)
		break
	}
	l.mu.Unlock()

	return &adk.GenResumeResult[Input, *schema.Message]{
		ResumeParams: params,
		Consumed:     interruptedItems,
	}, nil
}

// prepareAgent 每轮根据最新 ResearchState 构建系统提示词并创建 Agent。
//
// Supervisor 本质是一个 ReAct Agent：工具与治理中间件都在这里注入。
func (l *Loop) prepareAgent(ctx context.Context, _ *adk.TurnLoop[Input, *schema.Message],
	_ []Input) (adk.Agent, error) {
	cfg := &adk.ChatModelAgentConfig{
		Name:        "supervisor",
		Description: "research supervisor",
		Instruction: l.buildInstruction(ctx),
		Model:       l.model,
		// 中间件顺序即包裹顺序：审批（最外层）→ 错误兜底 → 动作上报（最内层）。
		Handlers: []adk.ChatModelAgentMiddleware{
			agenttool.NewApprovalMiddleware(l.policy),
			agenttool.NewSafeToolMiddleware(),
			agenttool.NewEventMiddleware(l),
		},
	}
	if len(l.tools) > 0 {
		cfg.ToolsConfig = adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: l.tools},
		}
	}
	return adk.NewChatModelAgent(ctx, cfg)
}

// buildInstruction 读取最新 ResearchState 并构建系统提示词。
func (l *Loop) buildInstruction(ctx context.Context) string {
	st, err := l.states.Get(ctx, l.taskID)
	if err != nil {
		return ""
	}
	return st.BuildContext()
}

// ─── agenttool.Recorder 实现：把工具动作上报为事件 ───────────────────────────

// ToolStarted 发布工具调用事件。
func (l *Loop) ToolStarted(name, arguments string) {
	l.publishEvent(event.ToolCallEvent{
		SessionID: l.sessionID,
		TaskID:    l.taskID,
		RunTaskID: l.runTaskID,
		ToolName:  name,
		Arguments: arguments,
	})
}

// ToolFinished 发布工具结果事件。
func (l *Loop) ToolFinished(name, output string, err error) {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	l.publishEvent(event.ToolResultEvent{
		SessionID: l.sessionID,
		TaskID:    l.taskID,
		RunTaskID: l.runTaskID,
		ToolName:  name,
		Output:    output,
		Err:       errMsg,
	})
}

// publishEvent 发布一个事件；总线为空时忽略。
func (l *Loop) publishEvent(data event.Event) {
	if l.bus == nil || data == nil {
		return
	}
	_ = l.bus.Publish(context.Background(), event.EventEnvelope{Data: data})
}

// onAgentEvents 消费 Agent 事件（流式）。
//
// ReAct 循环中模型可能在工具调用前后多次输出 assistant 文本：
//   - 每次输出都实时发布为 ModelOutput 事件（前端展示推理过程）；
//   - 只有"最后一段"assistant 文本作为 Finding（最终结论）。
//
// 收到中断事件（工具审批）时发布审批请求，并跳过本次 Commit。
func (l *Loop) onAgentEvents(ctx context.Context, tc *adk.TurnContext[Input, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent]) error {
	var streamed strings.Builder   // 全量输出（用于中断保存）
	var lastAnswer strings.Builder // 最近一段 assistant 文本（作为 Finding）
	interrupted := false

	for {
		event, ok := events.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			continue // 忽略错误事件（如抢占产生的 CancelError）
		}

		// 工具审批中断：记录中断点并通知前端，等待用户决定。
		if event.Action != nil && event.Action.Interrupted != nil {
			l.handleInterrupt(event.Action.Interrupted)
			interrupted = true
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mv := event.Output.MessageOutput

		// 工具结果到达：下一段 assistant 文本属于新一轮推理，重置"最终结论"。
		if mv.Role == schema.Tool {
			lastAnswer.Reset()
			continue
		}
		if mv.Role != schema.Assistant {
			continue
		}

		if mv.IsStreaming {
			if err := l.consumeStream(&streamed, &lastAnswer, mv.MessageStream); err != nil {
				break
			}
		} else if mv.Message != nil && mv.Message.Content != "" {
			streamed.WriteString(mv.Message.Content)
			lastAnswer.WriteString(mv.Message.Content)
		}

		if preempted(tc) {
			l.saveInterrupted(ctx, streamed.String())
			return nil
		}
	}

	if interrupted {
		// 等待审批：未完成的一轮不提交 Finding，也不发布 done（SSE 保持打开）。
		return nil
	}
	if preempted(tc) {
		l.saveInterrupted(ctx, streamed.String())
		return nil // 不发布 done：等待重新规划轮次继续产出
	}

	answer := lastAnswer.String()
	if strings.TrimSpace(answer) == "" {
		// 没有工具调用的普通场景：全量输出即最终结论
		answer = streamed.String()
	}

	err := l.commitFinding(ctx, answer)
	l.recordAssistantMessage(answer)
	l.publishOutputDone()
	l.publishResearchFinished()
	return err
}

// handleInterrupt 记录待审批的中断点并发布审批请求事件。
func (l *Loop) handleInterrupt(info *adk.InterruptInfo) {
	var id string
	var payload agenttool.ApprovalInfo
	var found bool
	for _, ic := range info.InterruptContexts {
		if ic == nil {
			continue
		}
		switch v := ic.Info.(type) {
		case agenttool.ApprovalInfo:
			payload, found = v, true
			if ic.IsRootCause || id == "" {
				id = ic.ID
			}
		case *agenttool.ApprovalInfo:
			if v != nil {
				payload, found = *v, true
				if ic.IsRootCause || id == "" {
					id = ic.ID
				}
			}
		}
		if found && ic.IsRootCause {
			break
		}
	}

	toolName, args := "", ""
	if found {
		toolName, args = payload.ToolName, payload.ArgumentsInJSON
	}

	l.mu.Lock()
	l.pendingInterruptID = id
	l.mu.Unlock()

	slog.Info("supervisor interrupted for tool approval",
		"task", l.taskID, "interrupt_id", id, "tool", toolName)

	l.publishEvent(event.ToolApprovalRequiredEvent{
		SessionID:   l.sessionID,
		TaskID:      l.taskID,
		RunTaskID:   l.runTaskID,
		ApprovalID:  id,
		ToolName:    toolName,
		Arguments:   args,
		Description: "Supervisor 请求执行工具 " + toolName + "，需要人工审批。",
	})
}

// consumeStream 逐块读取流式输出，同时累积到 streamed 与 lastAnswer，并发布 ModelOutput 事件。
func (l *Loop) consumeStream(streamed, lastAnswer *strings.Builder,
	sr *schema.StreamReader[*schema.Message]) error {
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
			streamed.WriteString(chunk.Content)
			lastAnswer.WriteString(chunk.Content)
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
		Data: event.ModelOutputEvent{SessionID: l.sessionID, RunTaskID: l.runTaskID, Chunk: chunk},
	})
}

// publishOutputDone 发布流式输出结束事件。
func (l *Loop) publishOutputDone() {
	if l.bus == nil {
		return
	}
	_ = l.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.ModelOutputDoneEvent{SessionID: l.sessionID, RunTaskID: l.runTaskID},
	})
}

// publishResearchFinished 发布研究完成事件，通知 Runtime 把 RunTask 置为 Finished。
func (l *Loop) publishResearchFinished() {
	if l.bus == nil {
		return
	}
	_ = l.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.ResearchFinishedEvent{
			SessionID: l.sessionID,
			TaskID:    l.taskID,
			RunTaskID: l.runTaskID,
		},
	})
}

// commitFinding 把模型回答作为 Finding Commit 到 ResearchState。
//
// Finding 只包含模型本轮产出；被中断的上下文（InterruptedContext）独立累积，
// 不由 Commit 消费——它会作为独立分区持续进入上下文构建。
func (l *Loop) commitFinding(ctx context.Context, answer string) error {
	if answer == "" {
		return nil
	}
	return l.states.Commit(ctx, l.taskID, researchstate.CommitInput{
		ResultContent:  answer,
		FindingContent: answer,
	})
}

// recordUserMessage 把用户消息写入 ResearchState 的对话历史；
// 若该任务尚无研究需求，则把这条消息作为需求。
func (l *Loop) recordUserMessage(message string) {
	if l.states == nil || message == "" {
		return
	}
	ctx := context.Background()
	if err := l.states.SetRequirementIfEmpty(ctx, l.taskID, message); err != nil &&
		!errors.Is(err, researchstate.ErrNotFound) {
		slog.Error("set research requirement failed", "task", l.taskID, "err", err)
	}
	if err := l.states.AppendMessage(ctx, l.taskID, researchstate.RoleUser, message); err != nil &&
		!errors.Is(err, researchstate.ErrNotFound) {
		slog.Error("append user message failed", "task", l.taskID, "err", err)
	}
}

// recordAssistantMessage 把 Supervisor 的完整回复写入对话历史。
func (l *Loop) recordAssistantMessage(answer string) {
	if l.states == nil || answer == "" {
		return
	}
	if err := l.states.AppendMessage(context.Background(), l.taskID,
		researchstate.RoleAssistant, answer); err != nil &&
		!errors.Is(err, researchstate.ErrNotFound) {
		slog.Error("append assistant message failed", "task", l.taskID, "err", err)
	}
}

// saveInterrupted 把中断前正在产生的上下文原文累积保存到 ResearchState。
func (l *Loop) saveInterrupted(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if err := l.states.AccumulateInterruptedContext(ctx, l.taskID, text); err != nil {
		slog.Error("accumulate interrupted context failed", "task", l.taskID, "err", err)
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
