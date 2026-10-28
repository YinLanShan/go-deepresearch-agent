// Package scheduler 实现 Scheduler：决定 Assignment "什么时候执行"。
//
// 边界（对应 docs/Architecture.md 5.6）：
//
//	Supervisor 决定研究什么 → Scheduler 决定什么时候执行 → Work 实际执行
//
// Scheduler 是受 Runtime 管理的独立生命周期实体：拥有自己的启动/停止、
// 自己的队列与并发控制，不参与 SupervisorLoop 的推理过程。
//
// 本阶段实现（最小链路）：
//   - Assignment 排队与并发控制；
//   - Worker 分配（委派 WorkPool 创建并执行 Work）；
//   - 全局中止时把未完成 Assignment 标记为 cancelled（不做真实中断）；
//   - 与 ResultAggregator 的槽位预留/提交确认协议。
//
// 暂不实现：优先级、依赖检查、超时、Retry、Resource Limit。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// WorkRunner 是 Scheduler 对 WorkPool 的最小依赖：提交一次 Work 执行。
type WorkRunner interface {
	Submit(ctx context.Context, w *domain.Work) error
	Cancel(runTaskID string) int
	// CancelByAssignment 取消某个 Assignment 正在执行的 Work（若有）。
	CancelByAssignment(runTaskID, assignmentID string) int
}

// ResultSlots 是 Scheduler 对 ResultAggregator 的最小依赖：
// 调度前预留结果接收槽位，取消时同步释放。
type ResultSlots interface {
	Reserve(assignment *domain.Assignment) error
	Cancel(assignmentID string)
	// Revoke 撤销一批 Assignment 的结果接收资格，并撤回已提交的结果。
	Revoke(runTaskID string, assignmentIDs []string, reason string) int
}

// Config 是 Scheduler 的配置。
type Config struct {
	Assignments storage.AssignmentStore
	Works       storage.WorkStore
	Pool        WorkRunner
	Slots       ResultSlots
	Bus         *event.Bus
	// MaxConcurrency 是"同时在途"的 Work 上限（排队中 + 执行中）。
	//
	// 它提供的是背压：防止 Scheduler 无限把 Work 塞进 WorkPool 队列。
	// 注意与 WorkPool 的 Workers（真正的并行执行槽位）区分：
	//   - MaxConcurrency 计数包含排队中的 Work；
	//   - Workers 只计正在执行的 Work。
	// 因此 MaxConcurrency 应 >= Workers，否则部分 Worker 会闲置。
	MaxConcurrency int
	Now            func() time.Time
	// Reporter 用于上报错误（写日志 + 按 Severity 决定是否推送前端）。
	// 为 nil 时只写日志。
	Reporter *fault.Reporter
}

// Scheduler 是 Assignment 的调度器。
type Scheduler struct {
	assignments storage.AssignmentStore
	works       storage.WorkStore
	pool        WorkRunner
	slots       ResultSlots
	bus         *event.Bus
	maxConc     int
	now         func() time.Time
	reporter    *fault.Reporter

	mu      sync.Mutex
	queue   []*domain.Assignment
	wake    chan struct{}
	running map[string]string        // assignmentID -> workID
	waiters map[string]chan struct{} // assignmentID -> Work 结束信号
	closed  bool

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// 编译期断言：Scheduler 参与 Runtime 的协商握手。
var (
	_ event.EventSender     = (*Scheduler)(nil)
	_ event.SyncEventSender = (*Scheduler)(nil)
)

// New 构造一个 Scheduler。
//
// MaxConcurrency 语义是"同时在途的 Work 上限"（排队 + 执行），
// 与 WorkPool 的 Workers（执行槽位）配合形成两级控制：前者做背压，后者定并行度。
func New(cfg Config) *Scheduler {
	maxConc := cfg.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 2
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Scheduler{
		assignments: cfg.Assignments,
		works:       cfg.Works,
		pool:        cfg.Pool,
		slots:       cfg.Slots,
		bus:         cfg.Bus,
		maxConc:     maxConc,
		now:         now,
		reporter:    cfg.Reporter,
		wake:        make(chan struct{}, 1),
		running:     make(map[string]string),
		waiters:     make(map[string]chan struct{}),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventAssignmentSubmitted, s)
		cfg.Bus.Subscribe(event.EventWorkUpdated, s)
		cfg.Bus.Subscribe(event.EventCoordinationRequested, s)
	}
	return s
}

// Start 启动调度循环（非阻塞）。
func (s *Scheduler) Start() {
	go s.loop()
}

// Shutdown 停止调度循环并等待退出。
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stopCh)
	})
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop 是调度主循环：从队列取 Assignment，受并发上限约束后交给 WorkPool。
func (s *Scheduler) loop() {
	defer close(s.doneCh)

	sem := make(chan struct{}, s.maxConc)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		next := s.pop()
		if next == nil {
			select {
			case <-s.stopCh:
				return
			case <-s.wake:
				continue
			}
		}

		// 等待并发容量（期间可被停止）。
		select {
		case sem <- struct{}{}:
		case <-s.stopCh:
			return
		}
		s.launch(next, sem)
	}
}

// launch 为一个 Assignment 创建 Work 并提交给 WorkPool。
//
// 并发容量（sem）在 Work 进入终态后才归还，因此 MaxConcurrency 限制的是
// "同时在途"的 Work——既包含已入队等待的，也包含正在执行的。
func (s *Scheduler) launch(a *domain.Assignment, sem chan struct{}) {
	// 先登记"Work 结束"信号，再提交执行，避免 Work 结束事件先于登记到达。
	done := make(chan struct{})
	s.mu.Lock()
	s.waiters[a.ID] = done
	s.mu.Unlock()

	// fail 处理提交失败：注销信号、标记失败并归还并发容量。
	fail := func(cause error) {
		s.mu.Lock()
		delete(s.waiters, a.ID)
		s.mu.Unlock()
		s.finishFailure(a, cause)
		<-sem
	}

	now := s.now()
	w := &domain.Work{
		ID:           ids.New(),
		AssignmentID: a.ID,
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		Goal:         a.Goal,
		Status:       domain.WorkPending,
		Attempt:      a.Attempt + 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.works.Create(context.Background(), w); err != nil {
		slog.Error("scheduler: create work failed", "assignment", a.ID, "err", err)
		fail(err)
		return
	}

	s.mu.Lock()
	s.running[a.ID] = w.ID
	s.mu.Unlock()

	a.Status = domain.AssignmentRunning
	a.Attempt = w.Attempt
	a.UpdatedAt = now
	s.updateOrInvalidate(a)

	s.publish(event.WorkCreatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		WorkID:       w.ID,
		Goal:         a.Goal,
	})
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
	})

	// 提交给 WorkPool 执行；执行结束由 WorkUpdated 事件回收容量。
	if err := s.pool.Submit(context.Background(), w); err != nil {
		slog.Error("scheduler: submit work failed", "work", w.ID, "err", err)
		fail(err)
		return
	}

	// 成功路径：等 Work 进入终态后归还并发容量。
	go func() {
		select {
		case <-done:
		case <-s.stopCh:
		}
		<-sem
	}()
}

// finishFailure 把 Assignment 置为 failed。
func (s *Scheduler) finishFailure(a *domain.Assignment, cause error) {
	a.Status = domain.AssignmentFailed
	a.Err = cause.Error()
	a.UpdatedAt = s.now()
	a.FinishedAt = a.UpdatedAt
	s.updateOrInvalidate(a)
	if s.slots != nil {
		s.slots.Cancel(a.ID)
	}
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Err:          a.Err,
	})
	s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
}

// pop 取出队首 Assignment；为空返回 nil。
func (s *Scheduler) pop() *domain.Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	a := s.queue[0]
	s.queue = s.queue[1:]
	return a
}

// wakeup 唤醒调度循环。
func (s *Scheduler) wakeup() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Send 处理异步事件。
func (s *Scheduler) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.AssignmentSubmittedEvent:
		s.enqueue(ev.Assignments)
	case event.WorkUpdatedEvent:
		s.handleWorkUpdated(ev)
	}
	return nil
}

// SendSync 参与 Runtime 的协商：
//   - ActionStop  ：全局中止时标记所有未完成 Assignment 为取消；
//   - ActionDelete：RunTask 被删除时，丢弃其排队条目并释放结果槽位。
//
// 注意：PublishSync 只会投递到 SyncEventSender，因此这里必须把
// 非协商事件委托给 Send，否则同步发布的 AssignmentSubmittedEvent 会被丢弃。
//
// 当前阶段只做状态标记，不做真实执行中断。
func (s *Scheduler) SendSync(ctx context.Context, e event.EventEnvelope, h *event.Handshake) error {
	req, ok := e.Data.(event.CoordinationRequestedEvent)
	if !ok {
		err := s.Send(ctx, e)
		h.Reply(err)
		return nil
	}
	switch req.Action {
	case event.ActionStop:
		s.cancelRunTask(req.RunTaskID)
	case event.ActionDelete:
		s.forgetRunTask(req.RunTaskID)
	}
	h.Reply(nil)
	return nil
}

// forgetRunTask 丢弃某个 RunTask 的排队条目并释放对应的结果槽位。
//
// Assignment 数据本身由 Runtime 通过 AssignmentStore.DeleteByRunTask 级联删除；
// 这里只清理 Scheduler 的内存状态，避免删除后仍被调度。
func (s *Scheduler) forgetRunTask(runTaskID string) {
	if runTaskID == "" {
		return
	}

	s.mu.Lock()
	kept := make([]*domain.Assignment, 0, len(s.queue))
	for _, a := range s.queue {
		if a.RunTaskID == runTaskID {
			continue
		}
		kept = append(kept, a)
	}
	s.queue = kept
	s.mu.Unlock()

	list, err := s.assignments.ListByRunTask(context.Background(), runTaskID)
	if err != nil {
		return
	}
	for _, a := range list {
		if s.slots != nil {
			s.slots.Cancel(a.ID)
		}
	}
}

// enqueue 登记并排队一批 Assignment。
func (s *Scheduler) enqueue(list []*domain.Assignment) {
	for _, a := range list {
		if a == nil {
			continue
		}
		// 1) 先预留结果接收槽位；预留失败不得进入调度队列。
		if s.slots != nil {
			if err := s.slots.Reserve(a); err != nil {
				// 标记为 cancelled 而不是静默跳过（Q4）：
				// 否则该 Assignment 既不会被调度、也没有终态，成为僵尸任务。
				s.failEnqueue(a, fault.E(fault.CodeSlotUnavailable, fault.LayerScheduler,
					"failed to reserve result slot").Severity(fault.SeverityError).
					Source("scheduler.enqueue").Wrap(err))
				continue
			}
		}

		a.Status = domain.AssignmentScheduled
		if a.UpdatedAt.IsZero() {
			a.UpdatedAt = s.now()
		}
		if err := s.assignments.Create(context.Background(), a); err != nil {
			slog.Error("scheduler: create assignment failed", "assignment", a.ID, "err", err)
			if s.slots != nil {
				s.slots.Cancel(a.ID)
			}
			s.report(fault.E(fault.CodeWorkCreateFailed, fault.LayerScheduler,
				"failed to persist assignment").Severity(fault.SeverityError).
				Source("scheduler.enqueue").Wrap(err), a)
			continue
		}

		s.publish(event.AssignmentUpdatedEvent{
			SessionID:    a.SessionID,
			TaskID:       a.TaskID,
			RunTaskID:    a.RunTaskID,
			AssignmentID: a.ID,
			Status:       a.Status,
		})

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.queue = append(s.queue, a)
		s.mu.Unlock()
	}
	s.wakeup()
}

// handleWorkUpdated 根据 Work 终态更新 Assignment 状态并归还容量。
func (s *Scheduler) handleWorkUpdated(ev event.WorkUpdatedEvent) {
	if !ev.Status.IsTerminal() {
		return
	}

	// 归还并发容量。
	s.mu.Lock()
	if ch, ok := s.waiters[ev.AssignmentID]; ok {
		close(ch)
		delete(s.waiters, ev.AssignmentID)
	}
	delete(s.running, ev.AssignmentID)
	s.mu.Unlock()

	a, err := s.assignments.Get(context.Background(), ev.AssignmentID)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			slog.Error("scheduler: get assignment failed", "assignment", ev.AssignmentID, "err", err)
		}
		return
	}
	if a.Status.IsTerminal() {
		return
	}

	switch ev.Status {
	case domain.WorkSucceeded:
		// 结果待 ResultAggregator 提交；提交确认后才置 completed。
		a.Status = domain.AssignmentAwaitingResult
	case domain.WorkFailed, domain.WorkTimedOut:
		a.Status = domain.AssignmentFailed
		a.Err = ev.Err
	case domain.WorkCancelled:
		a.Status = domain.AssignmentCancelled
		if s.slots != nil {
			s.slots.Cancel(a.ID)
		}
	}
	a.UpdatedAt = s.now()
	if a.Status.IsTerminal() || a.Status == domain.AssignmentAwaitingResult {
		if a.Status.IsTerminal() {
			a.FinishedAt = a.UpdatedAt
		}
	}
	s.updateOrInvalidate(a)
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Err:          a.Err,
	})
}

// OnResultCommitted 由 ResultAggregator 在 Commit 成功后调用：
// 只有此时才把 Assignment 置为 completed 并回收调度资源。
func (s *Scheduler) OnResultCommitted(assignmentID, result string) {
	a, err := s.assignments.Get(context.Background(), assignmentID)
	if err != nil {
		return
	}
	if a.Status == domain.AssignmentCompleted {
		return // 幂等
	}
	now := s.now()
	a.Status = domain.AssignmentCompleted
	a.Result = result
	a.UpdatedAt = now
	a.FinishedAt = now
	s.updateOrInvalidate(a)
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Result:       result,
	})
	s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
}

// maybePublishSettled 在某个 Assignment 进入终态后，检查该 RunTask 的子任务是否已全部终结。
//
// 若是则发布 AllAssignmentsSettledEvent：让空闲的 Supervisor 被唤醒一次去评估
// 是否该给出结论。若仍有非终态 Assignment，则什么都不做（保持原有的"结果提交唤醒"语义）。
func (s *Scheduler) maybePublishSettled(runTaskID, sessionID, taskID string) {
	if runTaskID == "" {
		return
	}
	list, err := s.assignments.ListByRunTask(context.Background(), runTaskID)
	if err != nil || len(list) == 0 {
		return
	}

	var succeeded, failed, cancelled int
	for _, a := range list {
		if !a.Status.IsTerminal() {
			return // 还有任务在跑或在排队
		}
		switch a.Status {
		case domain.AssignmentCompleted:
			succeeded++
		case domain.AssignmentFailed:
			failed++
		case domain.AssignmentCancelled:
			cancelled++
		}
	}

	s.publish(event.AllAssignmentsSettledEvent{
		SessionID: sessionID,
		TaskID:    taskID,
		RunTaskID: runTaskID,
		Total:     len(list),
		Succeeded: succeeded,
		Failed:    failed,
		Cancelled: cancelled,
	})
	slog.Info("scheduler: all assignments settled",
		"run_task", runTaskID, "total", len(list),
		"succeeded", succeeded, "failed", failed, "cancelled", cancelled)
}

// OnResultRejected 由 ResultAggregator 在 Commit 失败时调用：置为 failed。
func (s *Scheduler) OnResultRejected(assignmentID string, cause error) {
	a, err := s.assignments.Get(context.Background(), assignmentID)
	if err != nil {
		return
	}
	if a.Status.IsTerminal() {
		return
	}
	now := s.now()
	a.Status = domain.AssignmentFailed
	if cause != nil {
		a.Err = cause.Error()
	}
	a.UpdatedAt = now
	a.FinishedAt = now
	s.updateOrInvalidate(a)
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Err:          a.Err,
	})
	s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
}

// OnResultRevoked 由 ResultAggregator 在结果被撤销后调用：把 Assignment 置为 cancelled。
func (s *Scheduler) OnResultRevoked(assignmentID, reason string) {
	a, err := s.assignments.Get(context.Background(), assignmentID)
	if err != nil {
		return
	}
	if a.Status == domain.AssignmentCancelled {
		return // 幂等
	}
	now := s.now()
	a.Status = domain.AssignmentCancelled
	a.Result = ""
	if reason != "" {
		a.Err = reason
	}
	a.UpdatedAt = now
	a.FinishedAt = now
	s.updateOrInvalidate(a)
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Err:          a.Err,
	})
	s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
}

// CancelAssignments 按 ID 撤销一批 Assignment。
//
// 处理顺序遵循"先标记状态、再按状态处置"：
//  1. 先把 Assignment 标记为 cancelled（这是唯一的权威状态）；
//  2. 通知 ResultAggregator 撤销接收资格（二级索引），此后到达的结果一律被拦截；
//  3. 按撤销前的状态分别收尾：
//     - scheduled/pending（刚入队）：从调度队列移除；
//     - running（已调度/正在执行）：取消对应 Work；
//     - awaiting_result（等待提交）：结果被门控拦下，不会再进入 ResearchState；
//     - completed（已提交）：由 ResultAggregator 从 ResearchState 中撤回。
//
// 返回成功标记为 cancelled 的数量，以及从 ResearchState 中撤回的已提交结果条数。
func (s *Scheduler) CancelAssignments(runTaskID string, assignmentIDs []string, reason string) (cancelled int, revoked int, err error) {
	if runTaskID == "" || len(assignmentIDs) == 0 {
		return 0, 0, nil
	}

	target := make(map[string]struct{}, len(assignmentIDs))
	for _, id := range assignmentIDs {
		if id != "" {
			target[id] = struct{}{}
		}
	}
	if len(target) == 0 {
		return 0, 0, nil
	}

	// 1) 先标记状态：逐个加载、置 cancelled、写回。
	type previous struct {
		assignment *domain.Assignment
		status     domain.AssignmentStatus
	}
	prev := make([]previous, 0, len(target))
	now := s.now()
	for id := range target {
		a, err := s.assignments.Get(context.Background(), id)
		if err != nil {
			slog.Warn("scheduler: cancel unknown assignment", "assignment", id, "err", err)
			continue
		}
		if a.RunTaskID != runTaskID {
			slog.Warn("scheduler: assignment does not belong to run task",
				"assignment", id, "run_task", runTaskID, "actual", a.RunTaskID)
			continue
		}
		if a.Status == domain.AssignmentCancelled {
			continue // 幂等
		}
		prev = append(prev, previous{assignment: a, status: a.Status})

		a.Status = domain.AssignmentCancelled
		a.Result = ""
		if reason != "" {
			a.Err = reason
		}
		a.UpdatedAt = now
		a.FinishedAt = now
		// 撤销路径：状态写不进去时只上报，不再作废（它已经是终态）。
		if err := s.updateAssignment(a); err != nil {
			s.report(fault.E(fault.CodeInvalidTransition, fault.LayerScheduler,
				"failed to persist cancelled assignment").Severity(fault.SeverityError).
				Source("scheduler.CancelAssignments").Wrap(err), a)
		}
		s.publish(event.AssignmentUpdatedEvent{
			SessionID:    a.SessionID,
			TaskID:       a.TaskID,
			RunTaskID:    a.RunTaskID,
			AssignmentID: a.ID,
			Status:       a.Status,
			Err:          a.Err,
		})
	}

	if len(prev) == 0 {
		return 0, 0, nil
	}

	// 2) 通知结果聚合器撤销接收资格（先标记，后续结果被门控拦截）。
	ids := make([]string, 0, len(prev))
	for _, p := range prev {
		ids = append(ids, p.assignment.ID)
	}
	if s.slots != nil {
		revoked = s.slots.Revoke(runTaskID, ids, reason)
	}

	// 3) 按撤销前的状态收尾。
	needQueueClean := false
	for _, p := range prev {
		switch p.status {
		case domain.AssignmentScheduled, domain.AssignmentPending:
			needQueueClean = true // 刚入队：从调度队列移除
		case domain.AssignmentRunning:
			// 已调度/正在执行：取消 Work
			if s.pool != nil {
				s.pool.CancelByAssignment(runTaskID, p.assignment.ID)
			}
		case domain.AssignmentAwaitingResult:
			// 等待提交：结果已被门控拦截，无需额外动作
		case domain.AssignmentCompleted:
			// 已提交：由 ResultAggregator 从 ResearchState 撤回
		}
	}

	if needQueueClean {
		s.mu.Lock()
		kept := make([]*domain.Assignment, 0, len(s.queue))
		for _, a := range s.queue {
			if _, revoked := target[a.ID]; revoked {
				continue
			}
			kept = append(kept, a)
		}
		s.queue = kept
		s.mu.Unlock()
	}

	slog.Info("scheduler: assignments cancelled",
		"run_task", runTaskID, "count", len(prev), "revoked_results", revoked, "reason", reason)
	// 撤销也可能让该 RunTask 的子任务全部终结。
	if len(prev) > 0 {
		p := prev[0].assignment
		s.maybePublishSettled(runTaskID, p.SessionID, p.TaskID)
	}
	return len(prev), revoked, nil
}

// cancelRunTask 把某个 RunTask 下所有未完成 Assignment 标记为取消。
//
// 当前阶段只做状态标记（不进行真实执行中断）。
func (s *Scheduler) cancelRunTask(runTaskID string) {
	if runTaskID == "" {
		return
	}
	// 先清空内存队列中属于该 RunTask 的项。
	s.mu.Lock()
	kept := make([]*domain.Assignment, 0, len(s.queue))
	var dropped []*domain.Assignment
	for _, a := range s.queue {
		if a.RunTaskID == runTaskID {
			dropped = append(dropped, a)
			continue
		}
		kept = append(kept, a)
	}
	s.queue = kept
	s.mu.Unlock()

	for _, a := range dropped {
		s.markCancelled(a)
	}

	// 再处理已落库的未完成 Assignment（含正在运行的）。
	list, err := s.assignments.ListByRunTask(context.Background(), runTaskID)
	if err != nil {
		slog.Error("scheduler: list assignments failed", "run_task", runTaskID, "err", err)
		return
	}
	for _, a := range list {
		if a.Status.IsTerminal() {
			continue
		}
		s.markCancelled(a)
	}

	if s.pool != nil {
		s.pool.Cancel(runTaskID)
	}
}

// markCancelled 将 Assignment 置为 cancelled 并释放槽位。
func (s *Scheduler) markCancelled(a *domain.Assignment) {
	now := s.now()
	a.Status = domain.AssignmentCancelled
	a.UpdatedAt = now
	a.FinishedAt = now
	s.updateOrInvalidate(a)
	if s.slots != nil {
		s.slots.Cancel(a.ID)
	}
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
	})
}

// updateAssignment 更新 Assignment 状态：失败重试一次（Q3）。
//
// 仍失败时由调用方决定后续（多数场景会调用 invalidateAssignment 作废），
// 因为状态写不进存储意味着前端与实际不一致、重启后还可能丢数据。
func (s *Scheduler) updateAssignment(a *domain.Assignment) error {
	return fault.RetryOnce(context.Background(), func() error {
		return s.assignments.Update(context.Background(), a)
	})
}

// invalidateAssignment 把 Assignment 置为 failed 并作废（Q3）。
//
// 语义：该 Assignment 不再推进、结果不会被提交；同一 RunTask 的
// 其它 Assignment 不受影响。用于"状态持续写不进存储"这类不可恢复的情形。
func (s *Scheduler) invalidateAssignment(a *domain.Assignment, cause error) {
	if a == nil {
		return
	}
	now := s.now()
	a.Status = domain.AssignmentFailed
	a.UpdatedAt = now
	a.FinishedAt = now
	if cause != nil {
		if a.Err == "" {
			a.Err = cause.Error()
		}
	}
	// 作废也要尽力落库；失败只能上报（已无更弱的降级手段）。
	if err := s.updateAssignment(a); err != nil {
		slog.Error("scheduler: persist invalidated assignment failed",
			"assignment", a.ID, "err", err)
	}
	if s.slots != nil {
		s.slots.Cancel(a.ID)
	}
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Err:          a.Err,
	})
	s.report(fault.E(fault.CodeInvalidTransition, fault.LayerScheduler,
		"assignment invalidated due to persistent state write failure").
		Severity(fault.SeverityError).
		Source("scheduler.invalidateAssignment").
		Wrap(cause), a)
	if a.RunTaskID != "" {
		s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
	}
}

// updateOrInvalidate 更新状态；重试仍失败则作废该 Assignment（Q3）。
func (s *Scheduler) updateOrInvalidate(a *domain.Assignment) bool {
	if err := s.updateAssignment(a); err != nil {
		s.invalidateAssignment(a, err)
		return false
	}
	return true
}

// report 上报一个错误；reporter 为 nil 时只写日志。
func (s *Scheduler) report(err error, a *domain.Assignment) {
	if err == nil {
		return
	}
	scope := fault.Scope{Source: "scheduler"}
	if a != nil {
		scope = fault.Scope{
			SessionID:    a.SessionID,
			TaskID:       a.TaskID,
			RunTaskID:    a.RunTaskID,
			AssignmentID: a.ID,
			Source:       "scheduler",
		}
	}
	if s.reporter == nil {
		slog.Error("scheduler: unreported error",
			"assignment", scope.AssignmentID, "err", err)
		return
	}
	s.reporter.Report(context.Background(), err, scope)
}

// failEnqueue 在入队阶段失败时把 Assignment 标记为 cancelled 并上报（Q4）。
//
// 使用 cancelled 而不是 failed：这类失败发生在"开始执行之前"，
// 语义上等价于"从未派发"，与用户撤销的终态一致。
func (s *Scheduler) failEnqueue(a *domain.Assignment, cause error) {
	now := s.now()
	a.Status = domain.AssignmentCancelled
	a.UpdatedAt = now
	a.FinishedAt = now
	if cause != nil {
		a.Err = cause.Error()
	}
	// 尝试落库；失败也要上报（否则前端连这个任务存在过都看不到）。
	if err := fault.RetryOnce(context.Background(), func() error {
		return s.assignments.Create(context.Background(), a)
	}); err != nil {
		s.report(fault.E(fault.CodeWorkCreateFailed, fault.LayerScheduler,
			"failed to persist cancelled assignment").Severity(fault.SeverityError).
			Source("scheduler.failEnqueue").Wrap(err), a)
	} else {
		s.publish(event.AssignmentUpdatedEvent{
			SessionID:    a.SessionID,
			TaskID:       a.TaskID,
			RunTaskID:    a.RunTaskID,
			AssignmentID: a.ID,
			Status:       a.Status,
			Err:          a.Err,
		})
	}
	s.report(cause, a)
	if a.RunTaskID != "" {
		s.maybePublishSettled(a.RunTaskID, a.SessionID, a.TaskID)
	}
}

// publish 发布事件。
//
// 通知失败不影响主流程：事件只是观察通道，权威状态已落库。
// 因此这里显式忽略 Publish 的错误（符合错误手册第 7 节的约定）。
func (s *Scheduler) publish(data event.Event) {
	if s.bus == nil || data == nil {
		return
	}
	_ = s.bus.Publish(context.Background(), event.EventEnvelope{Data: data})
}

// Status 返回调度器当前状态（用于诊断与测试）。
func (s *Scheduler) Status() (queued, running int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue), len(s.running)
}

// ErrStopped 表示调度器已停止。
var ErrStopped = errors.New("scheduler: stopped")

// String 便于日志输出。
func (s *Scheduler) String() string {
	q, r := s.Status()
	return fmt.Sprintf("scheduler{queued=%d running=%d max=%d}", q, r, s.maxConc)
}
