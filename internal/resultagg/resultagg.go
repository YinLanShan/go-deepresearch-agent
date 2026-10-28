// Package resultagg 实现 ResultAggregator：Work 最终 Result 进入 ResearchState 的受控入口。
//
// 职责边界（对应 docs/Architecture.md 5.9 / 7.3 / 7.6）：
//   - 串行处理 Work 的最终 Result，不对多个 Work 并发整理；
//   - 按 AssignmentID 识别归属，先预留接收槽位，校验通过才提交；
//   - 把"当前已到达、已登记且满足提交条件"的多个 Result 合并为一次 Commit，
//     不设置人为等待窗口；
//   - Commit 成功后才通知 Scheduler 回收 Assignment，并通知 SupervisorLoop 刷新上下文。
//
// 本阶段实现（最小链路）：校验/归一/批量提交/通知。
// 暂不实现：去重、冲突处理、FindingGraph、上下文压缩。
package resultagg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// Committer 是向 ResearchState 提交结果的抽象（由 researchstate.Store 实现）。
type Committer interface {
	Commit(ctx context.Context, taskID string, in researchstate.CommitInput) error
	AppendResult(ctx context.Context, taskID string, in researchstate.CommitInput) error
	// RevokeResult 撤销某个 Assignment 已提交的 Result（返回撤销条数）。
	RevokeResult(ctx context.Context, taskID, assignmentID, reason string) (int, error)
}

// SchedulerNotifier 是 ResultAggregator 通知 Scheduler 的最小接口。
type SchedulerNotifier interface {
	OnResultCommitted(assignmentID, result string)
	OnResultRejected(assignmentID string, cause error)
	// OnResultRevoked 在结果被撤销（未提交即取消，或已提交后被撤回）后通知 Scheduler。
	OnResultRevoked(assignmentID, reason string)
}

// ErrSlotNotFound 表示结果对应的槽位不存在（未预留或已取消）。
var ErrSlotNotFound = errors.New("resultagg: assignment slot not found")

// pendingResult 是一条已登记、待提交的 Work 结果。
type pendingResult struct {
	AssignmentID string
	WorkID       string
	SessionID    string
	TaskID       string
	RunTaskID    string
	Result       string
	ReceivedAt   time.Time
}

// slot 是某个 Assignment 的结果接收槽位。
//
// 槽位生命周期与 Assignment 的撤销状态绑定：
//   - 撤销发生在提交前 → slot 标记 revoked，其后的结果一律被门控拦下；
//   - 撤销发生在提交后 → 通过 Committer.RevokeResult 从 ResearchState 中撤回。
type slot struct {
	Assignment *domain.Assignment
	Received   *pendingResult
	// Revoked 为 true 表示该 Assignment 已被撤销，不再接受任何结果。
	Revoked bool
	// Committed 为 true 表示该 Assignment 的结果已成功提交。
	Committed bool
}

// Config 是 ResultAggregator 的配置。
type Config struct {
	States    Committer
	Bus       *event.Bus
	Scheduler SchedulerNotifier
	Now       func() time.Time
	// Reporter 用于上报错误（写日志 + 按 Severity 决定是否推送前端）。
	// 为 nil 时只写日志。
	Reporter *fault.Reporter
}

// Aggregator 串行处理 Work 的最终 Result。
type Aggregator struct {
	states    Committer
	bus       *event.Bus
	scheduler SchedulerNotifier
	now       func() time.Time
	reporter  *fault.Reporter

	mu    sync.Mutex
	slots map[string]*slot // assignmentID -> slot
	// revokedRunTask 是"撤销门控"的二级索引：runTaskID -> 被撤销的 assignmentID 集合。
	//
	// 即使槽位已因提交而删除，也要能拦住该 Assignment 的重复/迟到结果，
	// 因此撤销状态按 RunTask 维度单独索引，并在 RunTask 结束时整体清理。
	revokedRunTask map[string]map[string]string // runTaskID -> assignmentID -> reason
	inbox          []*pendingResult
	wake           chan struct{}
	closed         bool

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// 编译期断言 participate 协商握手。
var (
	_ event.EventSender     = (*Aggregator)(nil)
	_ event.SyncEventSender = (*Aggregator)(nil)
)

// New 构造一个 ResultAggregator。
func New(cfg Config) *Aggregator {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	a := &Aggregator{
		states:         cfg.States,
		bus:            cfg.Bus,
		scheduler:      cfg.Scheduler,
		now:            now,
		reporter:       cfg.Reporter,
		slots:          make(map[string]*slot),
		revokedRunTask: make(map[string]map[string]string),
		wake:           make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventWorkResultReady, a)
		cfg.Bus.Subscribe(event.EventCoordinationRequested, a)
	}
	return a
}

// Start 启动串行处理循环（非阻塞）。
func (a *Aggregator) Start() { go a.loop() }

// SetSchedulerNotifier 注入 Scheduler 通知器。
//
// 单独提供 setter 是因为 Scheduler 与 Aggregator 互相引用
// （Scheduler 需要 Aggregator 预留槽位，Aggregator 需要回调 Scheduler 回收），
// 用 setter 打破构造期的循环依赖。
func (a *Aggregator) SetSchedulerNotifier(n SchedulerNotifier) { a.scheduler = n }

// Shutdown 停止处理循环。
func (a *Aggregator) Shutdown(ctx context.Context) error {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		a.mu.Unlock()
		close(a.stopCh)
	})
	select {
	case <-a.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reserve 为即将调度的 Assignment 预留结果接收槽位。
//
// 对应协议：槽位预留成功后，Assignment 才允许进入可调度状态。
func (a *Aggregator) Reserve(assignment *domain.Assignment) error {
	if assignment == nil || assignment.ID == "" {
		return errors.New("resultagg: assignment id is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.slots[assignment.ID]; exists {
		return fmt.Errorf("resultagg: slot %q already reserved", assignment.ID)
	}
	a.slots[assignment.ID] = &slot{Assignment: assignment}
	return nil
}

// Cancel 取消某个 Assignment 的接收槽位（Assignment 被取消时调用）。
func (a *Aggregator) Cancel(assignmentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.slots[assignmentID]; ok {
		s.Revoked = true
		delete(a.slots, assignmentID)
	}
}

// Revoke 撤销一批 Assignment 的结果接收资格。
//
// 语义要点（与"先标记、后拦截"一致）：
//  1. 先在锁内标记撤销状态（二级索引 runTaskID -> assignmentID），
//     提交侧据此拦截，此后到达的结果一律不被接受；
//  2. 清理尚未提交的待处理结果（inbox）与槽位；
//  3. 若该 Assignment 的结果此前已经合法提交，则从 ResearchState 中撤回
//     （撤销同样是一次提交动作，会写入提交历史）。
//
// 返回已被撤回的已提交结果条数。
func (a *Aggregator) Revoke(runTaskID string, assignmentIDs []string, reason string) int {
	if len(assignmentIDs) == 0 {
		return 0
	}

	// 1) 先标记状态（无论后续能否定位到槽位，都要记住"已撤销"）。
	a.mu.Lock()
	set, ok := a.revokedRunTask[runTaskID]
	if !ok {
		set = make(map[string]string)
		a.revokedRunTask[runTaskID] = set
	}
	committedTasks := make(map[string][]string) // taskID -> assignmentIDs（已提交，需要撤回）
	for _, id := range assignmentIDs {
		set[id] = reason
		if s, ok := a.slots[id]; ok {
			if s.Committed && s.Assignment != nil {
				committedTasks[s.Assignment.TaskID] = append(committedTasks[s.Assignment.TaskID], id)
			}
			s.Revoked = true
			delete(a.slots, id)
		}
	}
	// 2) 丢弃 inbox 中属于这些 Assignment 的待提交结果。
	if len(a.inbox) > 0 {
		kept := make([]*pendingResult, 0, len(a.inbox))
		for _, p := range a.inbox {
			if _, revoked := set[p.AssignmentID]; revoked {
				continue
			}
			kept = append(kept, p)
		}
		a.inbox = kept
	}
	a.mu.Unlock()

	// 3) 已提交的结果从 ResearchState 中撤回。
	revokedCount := 0
	for taskID, ids := range committedTasks {
		for _, id := range ids {
			n, err := a.states.RevokeResult(context.Background(), taskID, id, reason)
			if err != nil {
				// Q5：撤销失败会留下"本该删除却仍在 State 中"的结果，
				// 属数据不一致，升级为 fatal，但不阻断（研究可能已终结）。
				a.report(fault.E(fault.CodeCommitFailed, fault.LayerResult,
					"failed to revoke committed result").Severity(fault.SeverityFatal).
					Source("resultagg.Revoke").
					IDs(taskID, runTaskID, id, "").
					Wrap(err))
				continue
			}
			revokedCount += n
			if a.scheduler != nil {
				a.scheduler.OnResultRevoked(id, reason)
			}
		}
	}
	slog.Info("resultagg: assignments revoked",
		"run_task", runTaskID, "count", len(assignmentIDs), "committed_revoked", revokedCount)
	return revokedCount
}

// ForgotRunTask 清理某个 RunTask 的撤销索引与提交墓碑（RunTask 删除时调用）。
func (a *Aggregator) ForgotRunTask(runTaskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.revokedRunTask, runTaskID)
	for id, s := range a.slots {
		if s.Assignment != nil && s.Assignment.RunTaskID == runTaskID {
			delete(a.slots, id)
		}
	}
}

// Send 接收 Work 结果事件，登记到对应槽位。
func (a *Aggregator) Send(_ context.Context, e event.EventEnvelope) error {
	ev, ok := e.Data.(event.WorkResultReadyEvent)
	if !ok {
		return nil
	}
	a.register(&pendingResult{
		AssignmentID: ev.AssignmentID,
		WorkID:       ev.WorkID,
		SessionID:    ev.SessionID,
		TaskID:       ev.TaskID,
		RunTaskID:    ev.RunTaskID,
		Result:       ev.Result,
		ReceivedAt:   a.now(),
	})
	return nil
}

// SendSync 参与 Runtime 的协商：全局中止时丢弃该 RunTask 的待提交结果。
//
// 非协商事件委托给 Send，保证 PublishSync 投递的结果不会丢失。
func (a *Aggregator) SendSync(ctx context.Context, e event.EventEnvelope, h *event.Handshake) error {
	req, ok := e.Data.(event.CoordinationRequestedEvent)
	if !ok {
		err := a.Send(ctx, e)
		h.Reply(err)
		return nil
	}
	if req.Action == event.ActionStop {
		a.dropRunTask(req.RunTaskID)
	}
	h.Reply(nil)
	return nil
}

// register 登记一条待提交结果。
func (a *Aggregator) register(p *pendingResult) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.inbox = append(a.inbox, p)
	a.mu.Unlock()

	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// loop 是串行处理循环。
func (a *Aggregator) loop() {
	defer close(a.doneCh)
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		batch, ok := a.takeBatch()
		if len(batch) == 0 {
			if !ok {
				select {
				case <-a.stopCh:
					return
				case <-a.wake:
					continue
				}
			}
			continue
		}
		a.process(batch)
	}
}

// takeBatch 取出本轮可一起提交的结果。
//
// 批量边界：当前已到达且已登记的结果，不为等待潜在新结果而延迟。
// 同一 TaskID 的结果合并为一次 Commit。
func (a *Aggregator) takeBatch() (batch []*pendingResult, _ bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.inbox) == 0 {
		return nil, false
	}

	batch = a.inbox
	a.inbox = nil

	// 只保留仍有有效槽位的结果。
	valid := make([]*pendingResult, 0, len(batch))
	for _, p := range batch {
		s, ok := a.slots[p.AssignmentID]
		if !ok || s.Revoked || s.Committed {
			continue
		}
		if s.Received != nil && s.Received.WorkID == p.WorkID {
			continue // 幂等：同一 Work 的重复投递
		}
		s.Received = p
		valid = append(valid, p)
	}
	return valid, true
}

// process 串行提交一批结果。
func (a *Aggregator) process(batch []*pendingResult) {
	// 按 TaskID 分组：同一 ResearchState 的结果合并为一次 Commit。
	groups := make(map[string][]*pendingResult)
	order := make([]string, 0, len(batch))
	for _, p := range batch {
		if _, ok := groups[p.TaskID]; !ok {
			order = append(order, p.TaskID)
		}
		groups[p.TaskID] = append(groups[p.TaskID], p)
	}

	for _, taskID := range order {
		group := groups[taskID]
		if err := a.commitGroup(taskID, group); err != nil {
			// Commit 失败必须保留未提交结果、不得宣告成功（设计文档 4.x）。
			// 这里通知 Scheduler 置 Assignment 为 failed，并上报给前端。
			for _, p := range group {
				a.report(fault.E(fault.CodeCommitFailed, fault.LayerResult,
					"failed to commit research result").Severity(fault.SeverityError).
					Source("resultagg.process").
					IDs(taskID, p.RunTaskID, p.AssignmentID, p.WorkID).
					Wrap(err))
			}
			for _, p := range group {
				a.markRejected(p, err)
			}
			continue
		}
		for _, p := range group {
			a.markCommitted(p)
		}
		a.notifySupervisor(taskID, group)
	}
}

// commitGroup 把同一 Task 的多个结果作为独立单元提交到 ResearchState。
//
// 注意：多条 Result 各自作为 Result 切片的一个元素保存（不拼接成一个字符串），
// 这样才能按 Assignment 精确撤销。
//
// 提交前在锁内再次检查撤销门控：撤销优先标记状态，提交侧据此拦截，
// 保证"先标记、后拦截"的顺序语义。
func (a *Aggregator) commitGroup(taskID string, group []*pendingResult) error {
	if a.states == nil {
		return errors.New("resultagg: states not configured")
	}
	if len(group) == 0 {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	items := make([]researchstate.ResultInput, 0, len(group))
	empty, gated := 0, 0
	for _, p := range group {
		content := strings.TrimSpace(p.Result)
		if content == "" {
			empty++
			continue
		}
		// 撤销门控：已撤销的 Assignment 不允许再进入 ResearchState。
		if a.isRevokedLocked(p.RunTaskID, p.AssignmentID) {
			gated++
			continue
		}
		items = append(items, researchstate.ResultInput{
			AssignmentID: p.AssignmentID,
			Content:      content,
		})
	}
	if len(items) == 0 {
		if gated > 0 {
			// 全部被撤销门控拦下：这是预期行为，不是失败。
			return nil
		}
		return fmt.Errorf("resultagg: empty result for task %s", taskID)
	}

	// Finding 取本轮各结果的简要拼接，仅用于过程记录；Result 本身保持独立。
	finding := strings.Join(collectContents(items), "\n")

	return a.states.Commit(context.Background(), taskID, researchstate.CommitInput{
		Results:        items,
		FindingContent: finding,
	})
}

// collectContents 提取结果正文（仅用于生成 Finding 摘要）。
func collectContents(items []researchstate.ResultInput) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Content)
	}
	return out
}

// isRevokedLocked 判断某个 Assignment 是否已被撤销（调用方需持有 a.mu）。
func (a *Aggregator) isRevokedLocked(runTaskID, assignmentID string) bool {
	set, ok := a.revokedRunTask[runTaskID]
	if !ok {
		return false
	}
	_, revoked := set[assignmentID]
	return revoked
}

// markCommitted 标记槽位已提交并通知 Scheduler 回收。
//
// 注意：槽位不立即删除，而是保留为"墓碑"（Committed=true），
// 以便随后可能到来的撤销仍能定位到 TaskID 与 Assignment 信息。
// 墓碑在 RunTask 结束（dropRunTask / ForgotRunTask）或撤销时清理。
func (a *Aggregator) markCommitted(p *pendingResult) {
	a.mu.Lock()
	if s, ok := a.slots[p.AssignmentID]; ok {
		s.Committed = true
		s.Received = nil
	}
	a.mu.Unlock()

	if a.scheduler != nil {
		a.scheduler.OnResultCommitted(p.AssignmentID, p.Result)
	}
}

// markRejected 标记提交失败，保留错误以便重试或排查。
func (a *Aggregator) markRejected(p *pendingResult, cause error) {
	a.mu.Lock()
	if s, ok := a.slots[p.AssignmentID]; ok {
		s.Received = nil // 保留槽位，允许后续重试
	}
	a.mu.Unlock()

	if a.scheduler != nil {
		a.scheduler.OnResultRejected(p.AssignmentID, cause)
	}
}

// notifySupervisor 在 Commit 成功后通知 SupervisorLoop 刷新上下文。
//
// 通知只表示研究状态发生变化；权威数据仍来自 ResearchState。
func (a *Aggregator) notifySupervisor(taskID string, group []*pendingResult) {
	if a.bus == nil {
		return
	}
	version := int64(0)
	if v, ok := a.states.(interface {
		Get(context.Context, string) (*researchstate.State, error)
	}); ok {
		if st, err := v.Get(context.Background(), taskID); err == nil {
			version = st.Version
		}
	}
	sessionID := ""
	if len(group) > 0 {
		sessionID = group[0].SessionID
	}
	_ = a.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.ResearchStateCommittedEvent{
			SessionID: sessionID,
			TaskID:    taskID,
			Version:   version,
		},
	})
}

// report 上报一个错误；reporter 为 nil 时只写日志。
//
// 只负责上报，不改变业务状态（状态变更仍由调用点决定）。
func (a *Aggregator) report(err error) {
	if err == nil {
		return
	}
	if a.reporter == nil {
		slog.Error("resultagg: unreported error", "err", err)
		return
	}
	var fe *fault.Error
	scope := fault.Scope{Source: "resultagg"}
	if errors.As(err, &fe) {
		scope = fault.Scope{
			TaskID:       fe.TaskID,
			RunTaskID:    fe.RunTaskID,
			AssignmentID: fe.AssignmentID,
			WorkID:       fe.WorkID,
			Source:       "resultagg",
		}
	}
	a.reporter.Report(context.Background(), err, scope)
}

// dropRunTask 丢弃某个 RunTask 的待提交结果（全局中止）。
func (a *Aggregator) dropRunTask(runTaskID string) {
	if runTaskID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	kept := a.inbox[:0]
	for _, p := range a.inbox {
		if p.RunTaskID == runTaskID {
			continue
		}
		kept = append(kept, p)
	}
	a.inbox = kept

	for id, s := range a.slots {
		if s.Assignment != nil && s.Assignment.RunTaskID == runTaskID {
			delete(a.slots, id)
		}
	}
}

// Status 返回聚合器内部状态（用于诊断与测试）。
func (a *Aggregator) Status() (inbox, slots int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inbox), len(a.slots)
}
