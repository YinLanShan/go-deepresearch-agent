// Package workpool 实现 WorkPool：为 Assignment 的执行实例（Work）提供执行资源。
//
// 边界（对应 docs/Architecture.md 5.5/5.6）：
//
//	Assignment = What to do（Supervisor 决定）
//	Work       = One execution of that work（Scheduler 创建，WorkPool 执行）
//
// WorkPool 是受 Runtime 管理的独立生命周期实体：拥有固定数量的 Worker，
// 通过内部队列接收 Work，执行完成后把状态与最终 Result 交回。
//
// 本阶段实现（最小链路）：Work 内部运行 MockWorker，不接入真实模型。
// 暂不实现：超时中断、Retry、工具调用、真实 SubAgent。
package workpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// Worker 是执行 Work 的能力抽象。每个 Work 都应当获得独立实例。
//
// 注意：Worker 实例不是并发安全的，必须一个 Work 一个实例
// （子 Agent 内部持有自己的 AsyncIterator 与消息流）。
type Worker interface {
	// Run 执行一次 Work；执行过程中通过 progress 上报阶段变化。
	Run(ctx context.Context, w *domain.Work, progress func(stage, detail string)) (string, error)
	// ID 返回 Worker 标识。
	ID() string
}

// WorkerFactory 为每个 Work 创建独立的 Worker 实例。
//
// 采用工厂而非共享实例：子 Agent 的 Run 不可并发复用，
// 共享实例会让多个 Work 的 AsyncIterator 互相污染。
type WorkerFactory interface {
	// NewWorker 为一个 Work 创建执行器；w 已包含目标（Goal）等信息。
	NewWorker(ctx context.Context, w *domain.Work) (Worker, error)
}

// workerFunc 把函数适配为 WorkerFactory。
type workerFunc func(ctx context.Context, w *domain.Work) (Worker, error)

func (f workerFunc) NewWorker(ctx context.Context, w *domain.Work) (Worker, error) {
	return f(ctx, w)
}

// Config 是 WorkPool 的配置。
type Config struct {
	Works storage.WorkStore
	Bus   *event.Bus
	// Factory 为每个 Work 创建独立 Worker；Factory 与 Worker 二选一。
	Factory WorkerFactory
	// Worker 是固定 Worker（仅用于测试/单执行器场景）。
	// 设置后所有 Work 共享该实例，并发执行时不安全，生产环境请用 Factory。
	Worker  Worker
	Workers int
	// WorkTimeout 是单个 Work 的执行超时；<=0 时使用 DefaultWorkTimeout。
	WorkTimeout time.Duration
	Now         func() time.Time
	// Reporter 用于上报错误（写日志 + 按 Severity 决定是否推送前端）。
	// 为 nil 时只写日志。
	Reporter *fault.Reporter
}

// DefaultWorkTimeout 是 Work 的默认执行超时。
const DefaultWorkTimeout = 5 * time.Minute

// ErrWorkTimeout 表示 Work 执行超时（超时后子 Agent 已被取消）。
var ErrWorkTimeout = errors.New("workpool: work timed out")

// WorkPool 是 Work 的执行资源池。
type WorkPool struct {
	works       storage.WorkStore
	bus         *event.Bus
	factory     WorkerFactory
	worker      Worker
	workers     int
	workTimeout time.Duration
	now         func() time.Time
	reporter    *fault.Reporter

	mu      sync.Mutex
	queue   []*domain.Work
	cancels map[string]context.CancelFunc // workID -> cancel
	byRun   map[string]map[string]struct{}
	// stale 记录已进入终态的 Work，用于丢弃迟到的进度上报。
	stale  map[string]struct{}
	closed bool

	wake     chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// 编译期断言：WorkPool 参与 Runtime 的协商握手。
var (
	_ event.EventSender     = (*WorkPool)(nil)
	_ event.SyncEventSender = (*WorkPool)(nil)
)

// New 构造一个 WorkPool。Workers <= 0 时默认 2。
func New(cfg Config) *WorkPool {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	timeout := cfg.WorkTimeout
	if timeout <= 0 {
		timeout = DefaultWorkTimeout
	}
	// 只给了固定 Worker 时，包装成工厂，保持单执行器语义。
	factory := cfg.Factory
	if factory == nil && cfg.Worker != nil {
		w := cfg.Worker
		factory = workerFunc(func(context.Context, *domain.Work) (Worker, error) {
			return w, nil
		})
	}
	p := &WorkPool{
		works:       cfg.Works,
		bus:         cfg.Bus,
		factory:     factory,
		worker:      cfg.Worker,
		workers:     workers,
		workTimeout: timeout,
		now:         now,
		reporter:    cfg.Reporter,
		cancels:     make(map[string]context.CancelFunc),
		byRun:       make(map[string]map[string]struct{}),
		stale:       make(map[string]struct{}),
		wake:        make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
	if cfg.Bus != nil {
		cfg.Bus.Subscribe(event.EventCoordinationRequested, p)
	}
	return p
}

// Start 启动 Worker goroutine（非阻塞）。
func (p *WorkPool) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.workerLoop()
	}
	go func() {
		p.wg.Wait()
		close(p.doneCh)
	}()
}

// Shutdown 停止接收新 Work，并等待正在执行的 Work 结束。
func (p *WorkPool) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.stopCh)
	})
	select {
	case <-p.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Submit 把 Work 放入执行队列。
func (p *WorkPool) Submit(_ context.Context, w *domain.Work) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("workpool: closed")
	}
	p.queue = append(p.queue, w)
	if _, ok := p.byRun[w.RunTaskID]; !ok {
		p.byRun[w.RunTaskID] = make(map[string]struct{})
	}
	p.byRun[w.RunTaskID][w.ID] = struct{}{}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// CancelByAssignment 取消某个 Assignment 关联的 Work。
//
// 排队中的 Work 直接标记取消；执行中的 Work 通过 cancel 让 Worker 尽快退出
// （本阶段的 MockWorker 收到 ctx 取消后会停止推进）。
// 返回受影响（排队 + 执行中）的 Work 数量。
func (p *WorkPool) CancelByAssignment(runTaskID, assignmentID string) int {
	if assignmentID == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// 队列中尚未开始的 Work。
	kept := make([]*domain.Work, 0, len(p.queue))
	var dropped []*domain.Work
	for _, w := range p.queue {
		if w.AssignmentID == assignmentID && (runTaskID == "" || w.RunTaskID == runTaskID) {
			dropped = append(dropped, w)
			continue
		}
		kept = append(kept, w)
	}
	p.queue = kept

	// 执行中的 Work：取消其 ctx。
	running := 0
	for workID := range p.cancels {
		w, err := p.works.Get(context.Background(), workID)
		if err != nil || w.AssignmentID != assignmentID {
			continue
		}
		if runTaskID != "" && w.RunTaskID != runTaskID {
			continue
		}
		p.cancels[workID]()
		running++
	}

	for _, w := range dropped {
		go p.markCancelled(w)
	}
	if len(dropped)+running > 0 {
		slog.Info("workpool: cancelled works for assignment",
			"assignment", assignmentID, "queued", len(dropped), "running", running)
	}
	return len(dropped) + running
}

// Cancel 取消某个 RunTask 下的全部 Work（仅状态标记，不做真实执行中断）。
//
// 返回被标记取消的 Work 数量。
func (p *WorkPool) Cancel(runTaskID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if runTaskID == "" {
		return 0
	}
	ids, ok := p.byRun[runTaskID]
	if !ok {
		return 0
	}

	// 队列中尚未开始的 Work：直接丢弃并标记取消。
	kept := make([]*domain.Work, 0, len(p.queue))
	var dropped []*domain.Work
	for _, w := range p.queue {
		if w.RunTaskID == runTaskID {
			dropped = append(dropped, w)
			continue
		}
		kept = append(kept, w)
	}
	p.queue = kept

	// 正在执行的 Work：仅标记，不中断（本阶段要求）。
	var running []*domain.Work
	for id := range ids {
		cancel, ok := p.cancels[id]
		if !ok {
			continue
		}
		cancel() // 传给 Worker 的 ctx，MockWorker 会尽快退出
		if w, err := p.works.Get(context.Background(), id); err == nil && !w.Status.IsTerminal() {
			running = append(running, w)
		}
	}

	go func() {
		for _, w := range dropped {
			p.markCancelled(w)
		}
		for _, w := range running {
			p.markCancelled(w)
		}
	}()
	return len(dropped) + len(running)
}

// markCancelled 把 Work 置为 cancelled 并广播。
func (p *WorkPool) markCancelled(w *domain.Work) {
	// 先标记终态，阻止后续 progress 回调继续写库/推事件。
	p.markStale(w.ID)
	now := p.now()
	w.Status = domain.WorkCancelled
	w.UpdatedAt = now
	w.FinishedAt = now
	if err := p.updateWork(w); err != nil {
		p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
			"failed to persist work state").
			Severity(fault.SeverityError).
			Source("workpool.updateWork").
			Wrap(err), w, "workpool.updateWork")
	}
	p.publishWork(w)
}

// markStale 标记某个 Work 已进入终态，其后的进度上报一律忽略。
func (p *WorkPool) markStale(workID string) {
	p.mu.Lock()
	p.stale[workID] = struct{}{}
	p.mu.Unlock()
}

// isStale 判断某个 Work 是否已进入终态。
func (p *WorkPool) isStale(workID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.stale[workID]
	return ok
}

// clearStale 在 Work 收尾后清理标记，避免长期占用内存。
func (p *WorkPool) clearStale(workID string) {
	p.mu.Lock()
	delete(p.stale, workID)
	p.mu.Unlock()
}

// workerLoop 是单个 Worker 的取任务循环。
func (p *WorkPool) workerLoop() {
	defer p.wg.Done()
	for {
		w := p.pop()
		if w == nil {
			select {
			case <-p.stopCh:
				return
			case <-p.wake:
				continue
			}
		}
		p.run(w)
	}
}

// pop 取出队首 Work。
func (p *WorkPool) pop() *domain.Work {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil
	}
	w := p.queue[0]
	p.queue = p.queue[1:]
	return w
}

// run 执行一次 Work 并回写状态。
//
// 并发语义：每次执行都通过 factory 取一个独立 Worker 实例，
// 因此多个 Work 可以真正并行（各自持有独立的 Agent 与事件流）。
//
// 超时语义：为本次执行建立独立的 deadline ctx；超时后 cancel 该 ctx，
// 子 Agent 的模型调用与事件流会被一并中断（仅停止消费 Next() 不会让它停下）。
func (p *WorkPool) run(w *domain.Work) {
	ctx, cancel := context.WithTimeout(context.Background(), p.workTimeout)
	p.mu.Lock()
	p.cancels[w.ID] = cancel
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		delete(p.cancels, w.ID)
		if set, ok := p.byRun[w.RunTaskID]; ok {
			delete(set, w.ID)
			if len(set) == 0 {
				delete(p.byRun, w.RunTaskID)
			}
		}
		p.mu.Unlock()
	}()

	// 已被取消（入队后、开始前）则不再执行。
	current, err := p.works.Get(context.Background(), w.ID)
	if err == nil && current.Status.IsTerminal() {
		return
	}

	// 为本次执行创建独立 Worker。
	worker, err := p.newWorker(ctx, w)
	if err != nil {
		p.finishFailure(w, err)
		return
	}

	now := p.now()
	w.Status = domain.WorkRunning
	w.WorkerID = worker.ID()
	w.StartedAt = now
	w.UpdatedAt = now
	w.Stage = "starting"
	if err := p.updateWork(w); err != nil {
		p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
			"failed to persist work state").
			Severity(fault.SeverityError).
			Source("workpool.updateWork").
			Wrap(err), w, "workpool.updateWork")
	}
	p.publishWork(w)

	// 已进入终态后不再接受进度上报：取消/超时之后 Worker 仍可能回调 progress，
	// 若继续写库并推送，前端会收到"状态已 cancelled、阶段却还在推进"的错乱序列。
	var terminal atomic.Bool

	result, runErr := worker.Run(ctx, w, func(stage, detail string) {
		if terminal.Load() || p.isStale(w.ID) {
			return
		}
		w.Stage = stage
		w.Detail = detail
		w.UpdatedAt = p.now()
		if err := p.updateWork(w); err != nil {
			// 进度写库失败降级为 info：不影响最终结果提交，但要让用户知道
			// 前端进度可能与最终状态不同步。
			p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
				"failed to persist work progress").
				Severity(fault.SeverityInfo).
				Source("workpool.progress").
				Wrap(err), w, "workpool.progress")
		}
		p.publishWork(w)
	})
	terminal.Store(true)
	defer p.clearStale(w.ID)

	finish := p.now()
	w.FinishedAt = finish
	w.UpdatedAt = finish
	w.Stage = "finished"

	if runErr != nil {
		p.reportRunFailure(w, runErr)
		// 超时与取消都归为终态；超时额外标注原因，便于排查。
		if errors.Is(runErr, context.DeadlineExceeded) {
			w.Status = domain.WorkTimedOut
			w.Err = ErrWorkTimeout.Error()
		} else if errors.Is(runErr, context.Canceled) {
			w.Status = domain.WorkCancelled
		} else {
			w.Status = domain.WorkFailed
			w.Err = runErr.Error()
		}
		if err := p.updateWork(w); err != nil {
			p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
				"failed to persist terminal work state").
				Severity(fault.SeverityError).
				Source("workpool.run").
				Wrap(err), w, "workpool.run")
		}
		p.publishWork(w)
		return
	}

	w.Status = domain.WorkSucceeded
	w.Result = result
	if err := p.updateWork(w); err != nil {
		p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
			"failed to persist work state").
			Severity(fault.SeverityError).
			Source("workpool.updateWork").
			Wrap(err), w, "workpool.updateWork")
	}
	p.publishWork(w)

	// 结果交给 ResultAggregator 提交。
	p.publish(event.WorkResultReadyEvent{
		SessionID:    w.SessionID,
		TaskID:       w.TaskID,
		RunTaskID:    w.RunTaskID,
		AssignmentID: w.AssignmentID,
		WorkID:       w.ID,
		Result:       result,
	})
}

// newWorker 为一次 Work 执行创建独立 Worker。
func (p *WorkPool) newWorker(ctx context.Context, w *domain.Work) (Worker, error) {
	if p.factory == nil {
		return nil, errors.New("workpool: worker factory is not configured")
	}
	worker, err := p.factory.NewWorker(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("workpool: create worker: %w", err)
	}
	if worker == nil {
		return nil, errors.New("workpool: worker factory returned nil")
	}
	return worker, nil
}

// reportRunFailure 按失败原因上报 Work 层错误。
//
// 分类（对应错误手册 2.x）：
//   - 超时      → work_timeout   warning（结果丢弃，研究可继续）
//   - 取消      → work_canceled  info（控制流程的结果，不打扰用户）
//   - 空输出    → work_empty_result warning
//   - 需审批    → work_needs_approval error（防御性兜底）
//   - 其它      → work_failed    warning
func (p *WorkPool) reportRunFailure(w *domain.Work, runErr error) {
	const source = "workpool.run"

	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		p.report(fault.E(fault.CodeWorkTimeout, fault.LayerWork,
			"sub-agent execution timed out; its result was discarded").
			Severity(fault.SeverityWarning).
			Source(source).
			Retryable(false).
			Wrap(runErr), w, source)
	case errors.Is(runErr, context.Canceled):
		p.report(fault.E(fault.CodeWorkCanceled, fault.LayerWork,
			"sub-agent execution was cancelled").
			Severity(fault.SeverityInfo).
			Source(source).
			Wrap(runErr), w, source)
	case errors.Is(runErr, ErrSubAgentEmptyResult):
		p.report(fault.E(fault.CodeWorkEmptyResult, fault.LayerWork,
			"sub-agent produced no output").
			Severity(fault.SeverityWarning).
			Source(source).
			Wrap(runErr), w, source)
	case errors.Is(runErr, ErrSubAgentNeedsApproval):
		p.report(fault.E(fault.CodeWorkNeedsApproval, fault.LayerWork,
			"sub-agent was interrupted unexpectedly").
			Severity(fault.SeverityError).
			Source(source).
			Wrap(runErr), w, source)
	default:
		p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
			"sub-agent execution failed").
			Severity(fault.SeverityWarning).
			Source(source).
			Wrap(runErr), w, source)
	}
}

// report 上报一个错误；reporter 为 nil 时只写日志。
//
// 只负责上报，不改变业务状态（状态变更仍由调用点决定）。
func (p *WorkPool) report(err error, w *domain.Work, source string) {
	if err == nil {
		return
	}
	scope := fault.Scope{Source: source}
	if w != nil {
		scope = fault.Scope{
			SessionID:    w.SessionID,
			TaskID:       w.TaskID,
			RunTaskID:    w.RunTaskID,
			AssignmentID: w.AssignmentID,
			WorkID:       w.ID,
			Source:       source,
		}
	}
	if p.reporter == nil {
		slog.Error("workpool: unreported error", "source", source, "work", scope.WorkID, "err", err)
		return
	}
	p.reporter.Report(context.Background(), err, scope)
}

// updateWork 更新 Work 状态：失败重试一次，仍失败则上报（Q3）。
func (p *WorkPool) updateWork(w *domain.Work) error {
	return fault.RetryOnce(context.Background(), func() error {
		return p.works.Update(context.Background(), w)
	})
}

// finishFailure 在无法创建 Worker 等前置失败时收尾。
func (p *WorkPool) finishFailure(w *domain.Work, cause error) {
	now := p.now()
	w.Status = domain.WorkFailed
	w.Err = cause.Error()
	w.Stage = "finished"
	w.FinishedAt = now
	w.UpdatedAt = now
	if err := p.updateWork(w); err != nil {
		p.report(fault.E(fault.CodeWorkFailed, fault.LayerWork,
			"failed to persist work state").
			Severity(fault.SeverityError).
			Source("workpool.updateWork").
			Wrap(err), w, "workpool.updateWork")
	}
	p.publishWork(w)
}

// publishWork 广播 Work 状态变化（前端实时展示）。
func (p *WorkPool) publishWork(w *domain.Work) {
	p.publish(event.WorkUpdatedEvent{
		SessionID:    w.SessionID,
		TaskID:       w.TaskID,
		RunTaskID:    w.RunTaskID,
		AssignmentID: w.AssignmentID,
		WorkID:       w.ID,
		WorkerID:     w.WorkerID,
		Goal:         w.Goal,
		Status:       w.Status,
		Stage:        w.Stage,
		Detail:       w.Detail,
		Attempt:      w.Attempt,
		Err:          w.Err,
		StartedAt:    w.StartedAt,
		FinishedAt:   w.FinishedAt,
	})
}

// publish 发布事件。
//
// 通知失败不影响主流程：事件只是观察通道，权威状态已落库。
// 因此这里显式忽略 Publish 的错误（符合错误手册第 7 节的约定）。
func (p *WorkPool) publish(data event.Event) {
	if p.bus == nil || data == nil {
		return
	}
	_ = p.bus.Publish(context.Background(), event.EventEnvelope{Data: data})
}

// Send 处理异步事件（当前无需要）。
func (p *WorkPool) Send(_ context.Context, _ event.EventEnvelope) error { return nil }

// SendSync 参与 Runtime 的协商：
//   - ActionStop  ：全局中止时标记并停止该 RunTask 的 Work；
//   - ActionDelete：RunTask 被删除时，丢弃其排队条目并取消执行中的 Work。
//
// 非协商事件委托给 Send，保证 PublishSync 投递的事件不会丢失。
func (p *WorkPool) SendSync(ctx context.Context, e event.EventEnvelope, h *event.Handshake) error {
	req, ok := e.Data.(event.CoordinationRequestedEvent)
	if !ok {
		err := p.Send(ctx, e)
		h.Reply(err)
		return nil
	}
	switch req.Action {
	case event.ActionStop:
		n := p.Cancel(req.RunTaskID)
		if n > 0 {
			slog.Info("workpool: cancelled works on stop", "run_task", req.RunTaskID, "count", n)
		}
	case event.ActionDelete:
		p.forgetRunTask(req.RunTaskID)
	}
	h.Reply(nil)
	return nil
}

// forgetRunTask 丢弃某个 RunTask 的排队条目，并取消其执行中的 Work。
//
// Work 数据由 Runtime 通过 WorkStore.DeleteByRunTask 级联删除；
// 这里只保证删除后不再有新的执行被启动。
func (p *WorkPool) forgetRunTask(runTaskID string) {
	if runTaskID == "" {
		return
	}
	p.mu.Lock()
	kept := make([]*domain.Work, 0, len(p.queue))
	for _, w := range p.queue {
		if w.RunTaskID == runTaskID {
			continue
		}
		kept = append(kept, w)
	}
	p.queue = kept
	// 取消正在执行的 Work，避免删除后仍在后台写数据。
	for id := range p.byRun[runTaskID] {
		if cancel, ok := p.cancels[id]; ok {
			cancel()
		}
	}
	delete(p.byRun, runTaskID)
	p.mu.Unlock()
}

// Status 返回当前排队与执行中的 Work 数量（用于诊断与测试）。
func (p *WorkPool) Status() (queued, running int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue), len(p.cancels)
}
