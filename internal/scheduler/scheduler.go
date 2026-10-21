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
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// WorkRunner 是 Scheduler 对 WorkPool 的最小依赖：提交一次 Work 执行。
type WorkRunner interface {
	Submit(ctx context.Context, w *domain.Work) error
	Cancel(runTaskID string) int
}

// ResultSlots 是 Scheduler 对 ResultAggregator 的最小依赖：
// 调度前预留结果接收槽位，取消时同步释放。
type ResultSlots interface {
	Reserve(assignment *domain.Assignment) error
	Cancel(assignmentID string)
}

// Config 是 Scheduler 的配置。
type Config struct {
	Assignments    storage.AssignmentStore
	Works          storage.WorkStore
	Pool           WorkRunner
	Slots          ResultSlots
	Bus            *event.Bus
	MaxConcurrency int
	Now            func() time.Time
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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
	}

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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
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

// SendSync 参与 Runtime 的协商：全局中止时标记所有未完成 Assignment 为取消。
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
	if req.Action == event.ActionStop {
		s.cancelRunTask(req.RunTaskID)
	}
	h.Reply(nil)
	return nil
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
				slog.Error("scheduler: reserve result slot failed", "assignment", a.ID, "err", err)
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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
	}
	s.publish(event.AssignmentUpdatedEvent{
		SessionID:    a.SessionID,
		TaskID:       a.TaskID,
		RunTaskID:    a.RunTaskID,
		AssignmentID: a.ID,
		Status:       a.Status,
		Result:       result,
	})
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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
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
	if err := s.assignments.Update(context.Background(), a); err != nil {
		slog.Error("scheduler: update assignment failed", "assignment", a.ID, "err", err)
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
	})
}

// publish 发布事件。
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
