// Package event 定义进程内事件通信机制，对应 docs/Event-Error-Interrupt.md 第 11 节。
//
// 事件用于传递事实、状态变化与控制请求。本阶段提供：
//   - 事件接口与元数据（Event / EventMeta / EventEnvelope）；
//   - 进程内事件总线（Bus）：异步 Publish 与同步 PublishSync；
//   - 同步投递的握手机制（Handshake / SyncEventSender）。
//
// 设计要点：
//   - EventMeta 只含可序列化字段，不携带 ctx / chan 等运行时资源；
//   - "是否同步"是"这一次投递"的属性，由 Publish vs PublishSync 表达，不写进事件；
//   - Handshake 的通道均带 1 缓冲，双方都不 close，从根本上避免
//     "send on closed channel" 与"close 时仍有发送方阻塞"两类竞争。
package event

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// 事件类型常量。
const (
	// EventTaskEnabled 表示 Task 被启用，Runtime 应恢复对应的 RunTask。
	EventTaskEnabled = "task_enabled"
	// EventTaskDisabled 表示 Task 被禁用，Runtime 应暂停对应的 RunTask。
	EventTaskDisabled = "task_disabled"
	// EventRunTaskDeleteRequested 表示请求删除指定 Task 的 RunTask。
	EventRunTaskDeleteRequested = "run_task_delete_requested"
	// EventCoordinationRequested 表示 Runtime 向依赖组件发起的协商请求。
	EventCoordinationRequested = "coordination_requested"
	// EventUserMessage 表示用户向某 Session 的研究提交了一条对话消息。
	EventUserMessage = "user_message"
	// EventResearchStateCommitted 表示 ResearchState 出现一次新的 Commit。
	EventResearchStateCommitted = "research_state_committed"
	// EventStopRequested 表示用户主动请求中止当前研究运行。
	EventStopRequested = "stop_requested"
	// EventModelOutput 表示 Supervisor 流式输出的一块内容。
	EventModelOutput = "model_output"
	// EventModelOutputDone 表示本次研究（某条用户消息）的流式输出结束。
	EventModelOutputDone = "model_output_done"
	// EventResearchFinished 表示研究任务已完成（模型给出最终结论）。
	EventResearchFinished = "research_finished"
	// EventToolCall 表示 Supervisor 发起了一次工具调用。
	EventToolCall = "tool_call"
	// EventToolResult 表示一次工具调用返回了结果。
	EventToolResult = "tool_result"
	// EventToolApprovalRequired 表示某次工具调用需要人工审批后才能执行。
	EventToolApprovalRequired = "tool_approval_required"
	// EventToolApprovalCancelled 表示待审批的工具调用因任务中止而作废。
	EventToolApprovalCancelled = "tool_approval_cancelled"

	// EventAssignmentSubmitted 表示 Supervisor 提交了一个新的 Assignment。
	EventAssignmentSubmitted = "assignment_submitted"
	// EventAssignmentUpdated 表示 Assignment 的调度/执行状态发生变化。
	EventAssignmentUpdated = "assignment_updated"
	// EventWorkCreated 表示 WorkPool 为某个 Assignment 创建了一次执行实例。
	EventWorkCreated = "work_created"
	// EventWorkUpdated 表示 Work 的状态/阶段发生变化（前端实时展示）。
	EventWorkUpdated = "work_updated"
	// EventWorkResultReady 表示 Work 产出了最终 Result，待 ResultAggregator 提交。
	EventWorkResultReady = "work_result_ready"
	// EventRunStatusChanged 表示 RunTask 的状态发生变化（前端状态徽章实时更新）。
	EventRunStatusChanged = "run_status_changed"
	// EventAllAssignmentsSettled 表示某个 RunTask 下的 Assignment 已全部进入终态。
	//
	// 它填补一个缺口：当子任务全部失败/被撤销时，不会产生 WorkResultReady，
	// 也就不会有 ResearchStateCommitted，Supervisor 会永久空闲等待。
	// 该事件让空闲的 Supervisor 被唤醒一次，评估是否该给出结论。
	EventAllAssignmentsSettled = "all_assignments_settled"
)

// 协调动作（Runtime 发起的生命周期协商）。
const (
	ActionCreate = "create" // 协商创建并初始化新的 RunTask（含初始化要求）
	ActionResume = "resume" // 恢复运行
	ActionPause  = "pause"  // 暂停运行
	ActionStop   = "stop"   // 用户主动中止
	ActionDelete = "delete" // 删除
)

// Event 描述事件的业务类型及数据。
type Event interface {
	EventType() string
}

// Reply 是接收方完成处理后的回执。
type Reply struct {
	Err error
}

// EventMeta 提供统一的事件元数据。只含可序列化字段。
type EventMeta struct {
	EventID    string
	Source     string
	TaskID     string
	RunTaskID  string
	OccurredAt time.Time
}

// EventEnvelope 是携带元数据的事件。
type EventEnvelope struct {
	Meta EventMeta
	Data Event
}

// EventSender 接收事件（订阅者）。Send 返回 nil 只代表事件已被"接纳"，
// 不代表接收方已处理完成。
type EventSender interface {
	Send(ctx context.Context, e EventEnvelope) error
}

// SyncEventSender 是可同步确认的订阅者。
//
// SendSync 只负责把事件交给处理方（入队），必须立即返回；
// 处理完成由处理方调用 h.Reply(err) 回执，真正的等待由 Bus 通过 h.Wait 完成。
type SyncEventSender interface {
	SendSync(ctx context.Context, e EventEnvelope, h *Handshake) error
}

// Handshake 承载一次同步投递的三次握手。
//
//	发布方                            接收方
//	  |--- 事件 + Handshake ----------->|  1) 投递
//	  |<-- h.Reply(err) ----------------|  2) 处理完成，回执
//	  |--- h.Confirm() ---------------->|  3) 发布方确认（接收方据此释放）
//
// 两个通道都带 1 个缓冲，因此发送方永不阻塞、双方都不需要 close 通道。
type Handshake struct {
	reply   chan Reply
	confirm chan struct{}
	replyOn sync.Once
	confOn  sync.Once
}

// NewHandshake 由发布方创建一次握手。
func NewHandshake() *Handshake {
	return &Handshake{
		reply:   make(chan Reply, 1),
		confirm: make(chan struct{}),
	}
}

// Reply 由接收方调用一次，提交处理结果；重复调用被忽略。
func (h *Handshake) Reply(err error) {
	h.replyOn.Do(func() { h.reply <- Reply{Err: err} })
}

// Wait 由发布方等待回执。ctx 超时返回 ctx.Err()，不会把"没收到回执"当成成功。
func (h *Handshake) Wait(ctx context.Context) (Reply, error) {
	select {
	case r := <-h.reply:
		return r, nil
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

// Confirm 由发布方调用一次，结束本次握手。
// 无论等待成功还是超时都应调用，接收方才能解除等待并释放资源。
func (h *Handshake) Confirm() {
	h.confOn.Do(func() { close(h.confirm) })
}

// Confirmed 由接收方等待发布方确认。ctx 超时返回 ctx.Err()，避免无限期挂起。
func (h *Handshake) Confirmed(ctx context.Context) error {
	select {
	case <-h.confirm:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Bus 是进程内事件总线。
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]EventSender
}

// NewBus 构造一个事件总线。
func NewBus() *Bus {
	return &Bus{subs: make(map[string][]EventSender)}
}

// Subscribe 订阅指定事件类型。
func (b *Bus) Subscribe(eventType string, s EventSender) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], s)
}

// Unsubscribe 取消订阅（幂等）。
//
// 长连接类订阅者（如常驻 SSE）在连接关闭时必须调用它，
// 否则每次重新连接都会留下一个已失效的订阅者。
func (b *Bus) Unsubscribe(eventType string, s EventSender) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list, ok := b.subs[eventType]
	if !ok {
		return
	}
	kept := make([]EventSender, 0, len(list))
	for _, existing := range list {
		if existing == s {
			continue
		}
		kept = append(kept, existing)
	}
	if len(kept) == 0 {
		delete(b.subs, eventType)
		return
	}
	b.subs[eventType] = kept
}

// Publish 异步投递：只保证事件被接纳，不等待处理完成。
func (b *Bus) Publish(ctx context.Context, e EventEnvelope) error {
	return b.dispatch(ctx, e, false)
}

// PublishSync 同步投递：等待所有订阅者处理完成后再返回。
//
// 订阅者必须实现 SyncEventSender；否则返回错误，
// 而不是悄悄退化成异步（否则调用方会误以为对方已处理完成）。
func (b *Bus) PublishSync(ctx context.Context, e EventEnvelope) error {
	return b.dispatch(ctx, e, true)
}

func (b *Bus) dispatch(ctx context.Context, e EventEnvelope, syncMode bool) error {
	if e.Data == nil {
		return nil
	}
	b.mu.RLock()
	subs := make([]EventSender, len(b.subs[e.Data.EventType()]))
	copy(subs, b.subs[e.Data.EventType()])
	b.mu.RUnlock()

	var errs []error
	for _, s := range subs {
		if !syncMode {
			if err := s.Send(ctx, e); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		ss, ok := s.(SyncEventSender)
		if !ok {
			errs = append(errs, fmt.Errorf(
				"event: subscriber does not support sync delivery for %q", e.Data.EventType()))
			continue
		}

		h := NewHandshake()
		if err := ss.SendSync(ctx, e, h); err != nil {
			errs = append(errs, err)
			h.Confirm() // 投递失败也要确认，避免接收方（若已启动）悬挂
			continue
		}
		res, err := h.Wait(ctx)
		switch {
		case err != nil:
			errs = append(errs, err)
		case res.Err != nil:
			errs = append(errs, res.Err)
		}
		h.Confirm() // 无论成败都确认，接收方得以释放
	}
	return errors.Join(errs...)
}

// TaskEnabledEvent 表示 Task 被启用。
type TaskEnabledEvent struct {
	TaskID string
}

// EventType 实现 Event 接口。
func (TaskEnabledEvent) EventType() string { return EventTaskEnabled }

// TaskDisabledEvent 表示 Task 被禁用。
type TaskDisabledEvent struct {
	TaskID string
}

// EventType 实现 Event 接口。
func (TaskDisabledEvent) EventType() string { return EventTaskDisabled }

// RunTaskDeleteRequestedEvent 表示请求删除指定 Task 的 RunTask。
type RunTaskDeleteRequestedEvent struct {
	TaskID string
}

// EventType 实现 Event 接口。
func (RunTaskDeleteRequestedEvent) EventType() string { return EventRunTaskDeleteRequested }

// CoordinationRequestedEvent 是 Runtime 向依赖组件发起的协商请求。
type CoordinationRequestedEvent struct {
	TaskID    string
	RunTaskID string
	Action    string // ActionCreate / ActionResume / ActionPause / ActionDelete
}

// EventType 实现 Event 接口。
func (CoordinationRequestedEvent) EventType() string { return EventCoordinationRequested }

// UserMessageEvent 表示用户向某 Session 的研究提交了一条对话消息。
type UserMessageEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	Message   string
}

// EventType 实现 Event 接口。
func (UserMessageEvent) EventType() string { return EventUserMessage }

// ResearchStateCommittedEvent 表示 ResearchState 出现一次新的 Commit，
// 用于抢占正在推理的 SupervisorLoop。
type ResearchStateCommittedEvent struct {
	SessionID string
	TaskID    string
	Version   int64
}

// EventType 实现 Event 接口。
func (ResearchStateCommittedEvent) EventType() string { return EventResearchStateCommitted }

// StopRequestedEvent 表示用户主动请求中止当前研究运行。
type StopRequestedEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
}

// EventType 实现 Event 接口。
func (StopRequestedEvent) EventType() string { return EventStopRequested }

// ModelOutputEvent 表示 Supervisor 流式输出的一块内容。
type ModelOutputEvent struct {
	SessionID string
	RunTaskID string
	Chunk     string
}

// EventType 实现 Event 接口。
func (ModelOutputEvent) EventType() string { return EventModelOutput }

// ModelOutputDoneEvent 表示本次研究（某条用户消息）的流式输出结束。
type ModelOutputDoneEvent struct {
	SessionID string
	RunTaskID string
}

// EventType 实现 Event 接口。
func (ModelOutputDoneEvent) EventType() string { return EventModelOutputDone }

// ResearchFinishedEvent 表示研究任务已完成（模型给出最终结论）。
// Runtime 收到后把对应 RunTask 从 Running 置为 Finished。
type ResearchFinishedEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
}

// EventType 实现 Event 接口。
func (ResearchFinishedEvent) EventType() string { return EventResearchFinished }

// ToolCallEvent 表示 Supervisor 发起了一次工具调用。
type ToolCallEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	ToolName  string
	Arguments string
}

// EventType 实现 Event 接口。
func (ToolCallEvent) EventType() string { return EventToolCall }

// ToolResultEvent 表示一次工具调用返回了结果。
type ToolResultEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	ToolName  string
	Output    string
	Err       string
}

// EventType 实现 Event 接口。
func (ToolResultEvent) EventType() string { return EventToolResult }

// ToolApprovalRequiredEvent 表示某次工具调用需要人工审批。
// 前端据此展示审批卡片；用户决定后经 API 提交，工具调用方可继续。
type ToolApprovalRequiredEvent struct {
	SessionID   string
	TaskID      string
	RunTaskID   string
	ApprovalID  string
	ToolName    string
	Arguments   string
	Description string
}

// EventType 实现 Event 接口。
func (ToolApprovalRequiredEvent) EventType() string { return EventToolApprovalRequired }

// ToolApprovalCancelledEvent 表示待审批的工具调用已作废（任务被中止）。
// 前端据此把审批卡片置为"已失效"，避免用户误批。
type ToolApprovalCancelledEvent struct {
	SessionID  string
	TaskID     string
	RunTaskID  string
	ApprovalID string
	Reason     string
}

// EventType 实现 Event 接口。
func (ToolApprovalCancelledEvent) EventType() string { return EventToolApprovalCancelled }

// AssignmentSubmittedEvent 表示 Supervisor 提交了一批新的 Assignment。
type AssignmentSubmittedEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	// Assignments 是本次提交的 Assignment（已由调用方构造完整字段）。
	Assignments []*domain.Assignment
}

// EventType 实现 Event 接口。
func (AssignmentSubmittedEvent) EventType() string { return EventAssignmentSubmitted }

// AssignmentUpdatedEvent 表示某个 Assignment 的状态发生变化。
type AssignmentUpdatedEvent struct {
	SessionID    string
	TaskID       string
	RunTaskID    string
	AssignmentID string
	Status       domain.AssignmentStatus
	Result       string
	Err          string
}

// EventType 实现 Event 接口。
func (AssignmentUpdatedEvent) EventType() string { return EventAssignmentUpdated }

// WorkCreatedEvent 表示 WorkPool 创建了一次 Work 执行实例。
type WorkCreatedEvent struct {
	SessionID    string
	TaskID       string
	RunTaskID    string
	AssignmentID string
	WorkID       string
	WorkerID     string
	Goal         string
}

// EventType 实现 Event 接口。
func (WorkCreatedEvent) EventType() string { return EventWorkCreated }

// WorkUpdatedEvent 表示 Work 的状态或执行阶段发生变化，用于前端实时展示。
type WorkUpdatedEvent struct {
	SessionID    string
	TaskID       string
	RunTaskID    string
	AssignmentID string
	WorkID       string
	WorkerID     string
	Goal         string
	Status       domain.WorkStatus
	Stage        string
	Detail       string
	Attempt      int
	Err          string
	StartedAt    time.Time
	FinishedAt   time.Time
}

// EventType 实现 Event 接口。
func (WorkUpdatedEvent) EventType() string { return EventWorkUpdated }

// WorkResultReadyEvent 表示 Work 产出了最终 Result，等待 ResultAggregator 提交。
type WorkResultReadyEvent struct {
	SessionID    string
	TaskID       string
	RunTaskID    string
	AssignmentID string
	WorkID       string
	Result       string
}

// EventType 实现 Event 接口。
func (WorkResultReadyEvent) EventType() string { return EventWorkResultReady }

// RunStatusChangedEvent 表示 RunTask 的状态发生变化。
//
// 它是前端状态徽章的实时数据源：任何一次 RunTask 状态迁移
// （running / waiting / stopped / finished）都会发布该事件。
type RunStatusChangedEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	Status    domain.RunStatus
	// Previous 是变更前的状态（首次创建时为空）。
	Previous domain.RunStatus
}

// EventType 实现 Event 接口。
func (RunStatusChangedEvent) EventType() string { return EventRunStatusChanged }

// AllAssignmentsSettledEvent 表示某个 RunTask 下的 Assignment 已全部进入终态
// （completed / failed / cancelled）。
//
// 语义边界：它只表示"没有在跑或待跑的子任务了"，不表示研究结束——
// 是否结束仍由模型决定（finish_research）。
type AllAssignmentsSettledEvent struct {
	SessionID string
	TaskID    string
	RunTaskID string
	// Total 是该 RunTask 下的 Assignment 总数（含各类终态）。
	Total int
	// Succeeded 是其中已成功提交结果的数量。
	Succeeded int
	// Failed 是失败的数量。
	Failed int
	// Cancelled 是被撤销/取消的数量。
	Cancelled int
}

// EventType 实现 Event 接口。
func (AllAssignmentsSettledEvent) EventType() string { return EventAllAssignmentsSettled }
