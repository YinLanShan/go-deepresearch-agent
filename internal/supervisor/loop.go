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

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
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
	// Proposer 用于派发 Assignment（由 submit_assignments 工具触发）；为 nil 时不派发。
	Proposer AssignmentProposer
	// Canceller 用于撤销已派发的 Assignment（由 cancel_assignments 工具触发）；
	// 为 nil 时不支持撤销。
	Canceller AssignmentCanceller
	// Assignments 用于校验撤销请求中的 Assignment ID 归属；为 nil 时跳过归属校验。
	Assignments storage.AssignmentStore
}

// AssignmentProposer 由 Runtime 实现：接收模型通过 submit_assignments 提交的研究目标。
//
// 对应 docs/Architecture.md 5.6 的边界：Supervisor 决定"研究什么"，
// Scheduler 决定"什么时候执行"；Supervisor 不关心调度细节。
//
// 注意：派发是异步的，Supervisor 不会等待 Assignment 完成，
// 本轮可以正常结束；结果提交后由 ResearchStateCommitted 事件唤醒下一轮。
type AssignmentProposer interface {
	// ProposeAssignments 派发一批研究目标（goals 已由工具侧校验）。
	ProposeAssignments(ctx context.Context, taskID, runTaskID string, goals []string) (accepted int, err error)
}

// AssignmentCanceller 由 Runtime 实现：撤销模型此前派发的子任务。
//
// 允许范围是研究结束之前；撤销后槽位与 Work 立刻失效，
// 已提交的结果会从 ResearchState 中撤回（撤销同样是一次提交动作）。
type AssignmentCanceller interface {
	// CancelAssignments 撤销一批子任务，返回撤销数量与撤回的已提交结果条数。
	CancelAssignments(ctx context.Context, taskID, runTaskID string, assignmentIDs []string, reason string) (cancelled int, revoked int, err error)
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
	proposer    AssignmentProposer
	canceller   AssignmentCanceller
	assignments storage.AssignmentStore
	// finishing / finishConclusion 记录模型通过 finish_research 表达的终结意图。
	// 一旦置位不可撤销（不允许模型"反悔"）。
	finishing        bool
	finishConclusion string

	// turnActive 表示当前是否有轮次正在推理（true）还是空闲等待输入（false）。
	//
	// 它只用于内部判断"能否安全唤醒"，不暴露为 RunTask 状态：
	//   - 轮次运行中收到结果提交 → 走原有抢占机制；
	//   - 轮次空闲时收到"子任务全部终结" → 直接入队唤醒一次。
	turnActive bool
	// settledWoken 记录本次空闲期是否已因"子任务全部终结"唤醒过，避免重复唤醒。
	settledWoken bool

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

// 编译期断言：实现事件订阅、工具动作上报、"派发子任务"与"结束研究"接收接口。
var (
	_ event.EventSender  = (*Loop)(nil)
	_ agenttool.Recorder = (*Loop)(nil)
	_ agenttool.Assigner = (*Loop)(nil)
	_ agenttool.Finisher = (*Loop)(nil)
)

// RequestFinish 实现 agenttool.Finisher：模型调用 finish_research 时记录终结意图。
//
// 只置位、不立刻执行终止流程——终止必须在本轮收尾（onAgentEvents）里统一处理，
// 这样"模型声明结束"与"本轮实际结束"顺序确定，且调用后不会被新一轮推理推翻。
func (l *Loop) RequestFinish(conclusion string) {
	l.mu.Lock()
	if l.finishing {
		l.mu.Unlock()
		return
	}
	l.finishing = true
	l.finishConclusion = strings.TrimSpace(conclusion)
	l.mu.Unlock()

	slog.Info("supervisor: model requested finish", "task", l.taskID)
}

// finishingRequested 返回模型是否已声明研究结束。
func (l *Loop) finishingRequested() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.finishing, l.finishConclusion
}

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
		proposer:    cfg.Proposer,
		canceller:   cfg.Canceller,
		assignments: cfg.Assignments,
		resumeData:  make(map[string]agenttool.ApprovalResult),
	}
	l.loop = l.newTurnLoop()
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventUserMessage, l)
		cfg.Bus.Subscribe(event.EventResearchStateCommitted, l)
		// 子任务全部终结：让空闲的 Supervisor 被唤醒一次去评估是否该给出结论。
		cfg.Bus.Subscribe(event.EventAllAssignmentsSettled, l)
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
		// 记录用户消息到对话历史（同时作为需求累积）。
		l.recordUserMessage(ev.Message)
		// 用户消息拥有最高优先级：直接抢占当前轮次。
		// 被打断的那一轮输出会累积进 InterruptedContext，下一轮并入 Finding，
		// 这正是"结果延迟到下一轮提交"的表现。
		l.push(Input{Message: ev.Message}, adk.WithPreempt[Input, *schema.Message](adk.AnySafePoint))
	case event.ResearchStateCommittedEvent:
		if ev.TaskID != l.taskID {
			return nil
		}
		// 抢占当前轮次（在模型/工具调用间隙中断），并开启重新规划轮次。
		l.push(Input{Message: replanPrompt}, adk.WithPreempt[Input, *schema.Message](adk.AnySafePoint))
	case event.AllAssignmentsSettledEvent:
		if ev.RunTaskID != l.runTaskID {
			return nil
		}
		l.wakeOnSettled(ev)
	}
	return nil
}

// wakeOnSettled 在"子任务全部终结"时决定是否唤醒 Supervisor。
//
// 语义（按约定）：
//   - 轮次**运行中**：保持原有的抢占机制（结果提交会自己抢占），此处不额外入队，
//     避免同一时刻有两个输入争抢；
//   - 轮次**空闲**：直接入队一次唤醒，让模型评估是否该给出结论；
//   - 同一次空闲期只唤醒一次（settledWoken 去重），避免撤销/失败连续触发时反复唤醒。
func (l *Loop) wakeOnSettled(ev event.AllAssignmentsSettledEvent) {
	l.mu.Lock()
	if l.finishing {
		l.mu.Unlock()
		return // 已进入终结流程，无需唤醒
	}
	if l.turnActive {
		l.mu.Unlock()
		// 运行中：交给原有的抢占/结果提交机制，不重复入队。
		return
	}
	if l.settledWoken {
		l.mu.Unlock()
		return // 本次空闲期已唤醒过一次
	}
	l.settledWoken = true
	loop := l.loop
	l.mu.Unlock()

	slog.Info("supervisor: waking on settled assignments",
		"task", l.taskID, "total", ev.Total,
		"succeeded", ev.Succeeded, "failed", ev.Failed, "cancelled", ev.Cancelled)
	loop.Push(Input{Message: replanPrompt})
}

// markTurnActive / markTurnIdle 维护"本轮是否在推理"的内部标记。
//
// 进入 idle 时重置 settledWoken：新的一次空闲期允许再被"全部终结"唤醒一次。
func (l *Loop) markTurnActive() {
	l.mu.Lock()
	l.turnActive = true
	l.mu.Unlock()
}

func (l *Loop) markTurnIdle() {
	l.mu.Lock()
	l.turnActive = false
	l.settledWoken = false
	l.mu.Unlock()
}

// push 向当前 TurnLoop 投递输入。
func (l *Loop) push(item Input, opts ...adk.PushOption[Input, *schema.Message]) {
	l.mu.Lock()
	if l.finishing {
		// 模型已声明研究结束：不再接受任何新的推理输入（不可撤销）。
		l.mu.Unlock()
		return
	}
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
// prepareAgent 每轮根据最新 ResearchState 构建系统提示词并创建 Agent。
//
// Supervisor 本质是一个 ReAct Agent：工具与治理中间件都在这里注入。
// 除共享工具外，还会注入一个绑定当前 Loop 的 finish_research 工具——
// 它让模型显式声明"研究结束"，是本阶段唯一的终结入口。
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

	tools := make([]tool.BaseTool, 0, len(l.tools)+3)
	tools = append(tools, l.tools...)
	// 派发子任务：模型决定"研究什么"的唯一入口。
	if assignTool, err := agenttool.NewSubmitAssignmentsTool(l); err != nil {
		slog.Error("create submit_assignments tool failed", "task", l.taskID, "err", err)
	} else {
		tools = append(tools, assignTool)
	}
	// 撤销子任务：研究结束前，模型可以纠正自己的派发决定。
	if cancelTool, err := agenttool.NewCancelAssignmentsTool(l); err != nil {
		slog.Error("create cancel_assignments tool failed", "task", l.taskID, "err", err)
	} else {
		tools = append(tools, cancelTool)
	}
	// 宣布研究结束：研究进入终结态的唯一入口。
	if finishTool, err := agenttool.NewFinishResearchTool(l); err != nil {
		slog.Error("create finish_research tool failed", "task", l.taskID, "err", err)
	} else {
		tools = append(tools, finishTool)
	}
	if len(tools) > 0 {
		cfg.ToolsConfig = adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools},
		}
	}
	return adk.NewChatModelAgent(ctx, cfg)
}

// buildInstruction 读取最新 ResearchState 并构建系统提示词。
//
// 除研究状态外，还会附上本 RunTask 的子任务清单（含 Assignment ID），
// 因为模型需要一个稳定的 ID 才能在 cancel_assignments 中精确撤销。
func (l *Loop) buildInstruction(ctx context.Context) string {
	st, err := l.states.Get(ctx, l.taskID)
	if err != nil {
		return ""
	}
	return st.BuildContext() + l.assignmentsSection(ctx)
}

// assignmentsSection 渲染本 RunTask 的子任务清单（供模型决定是否撤销）。
func (l *Loop) assignmentsSection(ctx context.Context) string {
	if l.assignments == nil || l.runTaskID == "" {
		return ""
	}
	list, err := l.assignments.ListByRunTask(ctx, l.runTaskID)
	if err != nil || len(list) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n# 子任务清单（Assignment）\n\n")
	b.WriteString("下面是本次运行已派发的子任务及其当前状态。\n")
	b.WriteString("若某个子任务不再需要、目标写错或与其他子任务重复，" +
		"可调用 cancel_assignments 用其 ID 撤销（研究结束前有效）。\n\n")
	for _, a := range list {
		fmt.Fprintf(&b, "- ID: %s | 状态: %s | 目标: %s\n",
			a.ID, assignmentStatusText(a.Status), oneLine(a.Goal))
	}
	return b.String()
}

// assignmentStatusText 把 Assignment 状态翻译为面向模型的中文说明。
func assignmentStatusText(s domain.AssignmentStatus) string {
	switch s {
	case domain.AssignmentPending:
		return "待调度"
	case domain.AssignmentScheduled:
		return "已入队"
	case domain.AssignmentRunning:
		return "执行中"
	case domain.AssignmentAwaitingResult:
		return "等待提交"
	case domain.AssignmentCompleted:
		return "已完成（结果已提交）"
	case domain.AssignmentFailed:
		return "失败"
	case domain.AssignmentCancelled:
		return "已撤销"
	default:
		return string(s)
	}
}

// oneLine 把多行文本压成一行（避免子任务目标撑坏清单结构）。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
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
//   - Finding 的语义是"除工具调用外的全部模型输出"，即多段叙述的拼接。
//
// 收到中断事件（工具审批）时发布审批请求，并跳过本次 Commit。
func (l *Loop) onAgentEvents(ctx context.Context, tc *adk.TurnContext[Input, *schema.Message],
	events *adk.AsyncIterator[*adk.AgentEvent]) error {
	// 本轮开始：标记为运行中，并保证退出时恢复空闲。
	l.markTurnActive()
	defer l.markTurnIdle()

	// answer 累积本轮全部 assistant 文本（跳过 tool 消息），作为 Finding。
	var answer strings.Builder
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

		// 工具结果不属于模型输出，跳过（但不清空已累积的叙述）。
		if mv.Role == schema.Tool {
			continue
		}
		if mv.Role != schema.Assistant {
			continue
		}

		if mv.IsStreaming {
			if err := l.consumeStream(&answer, mv.MessageStream); err != nil {
				break
			}
		} else if mv.Message != nil && mv.Message.Content != "" {
			answer.WriteString(mv.Message.Content)
		}

		if preempted(tc) {
			l.saveInterrupted(ctx, answer.String())
			return nil
		}
	}

	if interrupted {
		// 等待审批：未完成的一轮不提交 Finding，也不发布 done（SSE 保持打开）。
		return nil
	}
	if preempted(tc) {
		l.saveInterrupted(ctx, answer.String())
		return nil // 不发布 done：等待重新规划轮次继续产出
	}

	// 本轮过程输出（工具调用前的叙述 + 调用后的叙述）。
	finding := l.buildFindingContent(ctx, answer.String())
	finishing, conclusion := l.finishingRequested()

	err := l.commitFinding(ctx, finding)
	l.recordAssistantMessage(finding)

	if finishing {
		// 研究终结：把模型给出的最终结论写入独立字段 Conclusion，
		// 再取消所有后台任务、关闭提交门控，由 Runtime 置 RunTask 为 finished。
		if strings.TrimSpace(conclusion) != "" {
			if err := l.states.SetConclusion(ctx, l.taskID, conclusion); err != nil {
				slog.Error("set conclusion failed", "task", l.taskID, "err", err)
			}
		}
		l.publishOutputDone()
		l.publishResearchFinished()
		return err
	}

	// 本轮结束（非终结）：Assignment 已由模型通过 submit_assignments 工具派发，
	// 这里只需结束本轮即可，不等待子任务完成。
	// RunTask 保持 running，Loop 保留；结果提交后由 ResearchStateCommitted 唤醒下一轮。
	l.publishOutputDone()
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
// consumeStream 逐块读取流式输出，累积到 answer 并发布 ModelOutput 事件。
func (l *Loop) consumeStream(answer *strings.Builder,
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
			answer.WriteString(chunk.Content)
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

// proposeAssignments 在轮次结束后向 Runtime 提交新的研究目标（Assignment）。
//
// 本阶段为最小链路：每个 Loop 只在首轮派发一次，避免形成无限扩增。
// Supervisor 只负责"研究什么"，调度由 Scheduler 决定。
// SubmitAssignments 实现 agenttool.Assigner：模型调用 submit_assignments 时派发子任务。
//
// 是否派发、派发几个、目标是什么，全部由模型决定；这里只做转交与计数。
func (l *Loop) SubmitAssignments(ctx context.Context, goals []string) (int, []string, error) {
	if l.proposer == nil {
		return 0, nil, errors.New("assignments are not supported in this loop")
	}
	// 已进入终结流程时不再接受新的子任务。
	l.mu.Lock()
	finishing := l.finishing
	l.mu.Unlock()
	if finishing {
		return 0, []string{"研究已进入结束流程，不再派发新子任务"}, nil
	}

	accepted, err := l.proposer.ProposeAssignments(ctx, l.taskID, l.runTaskID, goals)
	if err != nil {
		return 0, nil, err
	}
	slog.Info("supervisor: assignments submitted", "task", l.taskID, "accepted", accepted)
	return accepted, nil, nil
}

// CancelAssignments 实现 agenttool.Canceller：撤销模型此前派发的子任务。
//
// 允许范围：研究结束（finish_research）之前。进入结束流程后拒绝新的撤销请求，
// 因为此时所有子任务已在终结流程中被统一取消。
func (l *Loop) CancelAssignments(ctx context.Context, assignmentIDs []string,
	reason string) (cancelled int, revoked int, rejected []string, err error) {
	if l.canceller == nil {
		return 0, 0, nil, errors.New("cancel assignments is not supported in this loop")
	}
	l.mu.Lock()
	finishing := l.finishing
	l.mu.Unlock()
	if finishing {
		return 0, 0, []string{"研究已进入结束流程，所有子任务已被取消"}, nil
	}
	if len(assignmentIDs) == 0 {
		return 0, 0, nil, nil
	}

	// 只允许撤销本 RunTask 下、仍属于该 Task 的 Assignment。
	owned := make(map[string]*domain.Assignment)
	if l.assignments != nil {
		list, err := l.assignments.ListByRunTask(ctx, l.runTaskID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return 0, 0, nil, err
		}
		for _, a := range list {
			owned[a.ID] = a
		}
	}

	valid := make([]string, 0, len(assignmentIDs))
	for _, id := range assignmentIDs {
		a, ok := owned[id]
		if !ok {
			rejected = append(rejected, fmt.Sprintf("未找到属于当前运行的子任务 %q", id))
			continue
		}
		if a.Status == domain.AssignmentCancelled {
			continue // 已经是撤销态，幂等
		}
		valid = append(valid, id)
	}
	if len(valid) == 0 {
		return 0, 0, rejected, nil
	}

	cancelled, revoked, err = l.canceller.CancelAssignments(ctx, l.taskID, l.runTaskID, valid, reason)
	if err != nil {
		return 0, 0, rejected, err
	}
	slog.Info("supervisor: assignments cancelled",
		"task", l.taskID, "cancelled", cancelled, "revoked_results", revoked, "reason", reason)
	return cancelled, revoked, rejected, nil
}

// commitFinding 把模型本轮的过程输出作为 Finding Commit 到 ResearchState。
//
// Finding 的语义是"除工具调用外的全部模型输出"；若上一轮被新提交打断，
// 被打断的原文会拼在本轮 Finding 之前（即"重写被打断的 Finding"）。
// Commit 成功后，Store 会清空 InterruptedContext（中间产物）。
//
// 注意：这里不产生 Result —— Result 是子任务（Assignment）产出的证据，
// Supervisor 自身的输出只属于过程记录（Finding）。
func (l *Loop) commitFinding(ctx context.Context, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return l.states.Commit(ctx, l.taskID, researchstate.CommitInput{
		FindingContent: content,
	})
}

// buildFindingContent 组装本轮 Finding：中断原文（若有）+ 本轮输出。
func (l *Loop) buildFindingContent(ctx context.Context, answer string) string {
	st, err := l.states.Get(ctx, l.taskID)
	if err != nil {
		return answer
	}
	interrupted := strings.TrimSpace(st.InterruptedContext)
	answer = strings.TrimSpace(answer)
	switch {
	case interrupted == "":
		return answer
	case answer == "":
		return interrupted
	default:
		return interrupted + "\n" + answer
	}
}

// recordUserMessage 把用户消息追加为研究需求，并写入对话历史。
//
// Requirements 是复数语义：一个 Task 内用户的所有消息都按顺序保留。
func (l *Loop) recordUserMessage(message string) {
	if l.states == nil || strings.TrimSpace(message) == "" {
		return
	}
	message = strings.TrimSpace(message)
	ctx := context.Background()
	if err := l.states.AddRequirement(ctx, l.taskID, message); err != nil &&
		!errors.Is(err, researchstate.ErrNotFound) {
		slog.Error("add research requirement failed", "task", l.taskID, "err", err)
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
