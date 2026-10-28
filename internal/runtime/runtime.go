// Package runtime 提供维护 RunTask 生命周期的独立实体 Runtime。
//
// Runtime 是 RunTask 的运行时承载与控制核心（对应 docs/Architecture.md 6.2 节的
// Runtime Core，本阶段实现其生命周期骨架）。它订阅事件总线的控制事件，在独立
// goroutine 中串行处理事件。
//
// 关键边界：
//   - RunTask 不由 HTTP Handler 直接创建或变更；用户操作以事件提交到 Runtime。
//   - Runtime 收到变更事件后，先向依赖组件（Supervisor / Scheduler / WorkPool 等，
//     本阶段为 Mock）发起协商，协商全部成功后，才原子地落地内存状态。
//   - RunTask 启动（创建）时，Runtime 初始化 ResearchState（按 TaskID）与 SupervisorLoop。
//
// 生命周期语义（本阶段）：
//   - 一个 Session 可对应多个 Task，一个 Task 对应一个 RunTask；
//   - 全局同一时间只有一个 RunTask 处于 Running（使能新 Session 前先失能旧 Session）；
//   - 失能 = 全局中止 + RunTask 置 Paused（可经"继续"恢复为 Running）；
//   - 用户主动中止 = 全局中止 + RunTask 置 Stopped（终止态，发新消息会新建 Task）；
//   - 模型完成研究 = RunTask 置 Finished（终止态，发新消息会新建 Task）。
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/resultagg"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/scheduler"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/supervisor"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/workpool"
)

// delivery 是一次入队投递：env 为事件，h 为握手（异步投递时 h 为 nil）。
type delivery struct {
	env event.EventEnvelope
	h   *event.Handshake
}

// Runtime 是维护 RunTask 生命周期的独立实体。
type Runtime struct {
	runs   storage.RunTaskStore
	tasks  storage.TaskStore
	states researchstate.Store
	model  model.BaseChatModel
	bus    *event.Bus

	// tools 是 Supervisor（ReAct Agent）可用的工具（MCP / 内置），可为 nil。
	tools *agenttool.Registry
	// checkPoints 用于中断时的 Checkpoint 序列化（序列化与恢复语义由 Eino ADK 负责）。
	checkPoints adk.CheckPointStore

	// supervisors 维护每个运行中 Task 的 SupervisorLoop（按 TaskID）。
	mu          sync.Mutex
	supervisors map[string]*supervisor.Loop

	// 以下为受 Runtime 管理的独立生命周期实体（Scheduler / WorkPool / ResultAggregator）。
	// 它们各自拥有启动与停止流程，不参与 SupervisorLoop 的推理过程。
	assignments storage.AssignmentStore
	works       storage.WorkStore
	scheduler   *scheduler.Scheduler
	workPool    *workpool.WorkPool
	aggregator  *resultagg.Aggregator

	// reporter 统一上报错误（写日志 + 按 Severity 决定是否推送前端）。
	// 为 nil 时只写日志，不影响主流程。
	reporter *fault.Reporter

	events chan delivery
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	now    func() time.Time
}

// 编译期断言实现两个投递接口。
var (
	_ event.EventSender     = (*Runtime)(nil)
	_ event.SyncEventSender = (*Runtime)(nil)
)

// New 构造一个 Runtime，并订阅事件总线的控制事件。
func New(runs storage.RunTaskStore, tasks storage.TaskStore, states researchstate.Store,
	model model.BaseChatModel, bus *event.Bus) *Runtime {
	r := &Runtime{
		runs:        runs,
		tasks:       tasks,
		states:      states,
		model:       model,
		bus:         bus,
		supervisors: make(map[string]*supervisor.Loop),
		events:      make(chan delivery, 128),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		now:         time.Now,
	}
	if bus != nil {
		bus.Subscribe(event.EventTaskEnabled, r)
		bus.Subscribe(event.EventTaskDisabled, r)
		bus.Subscribe(event.EventRunTaskDeleteRequested, r)
		bus.Subscribe(event.EventStopRequested, r)
		bus.Subscribe(event.EventResearchFinished, r)
	}
	return r
}

// Start 启动 Runtime 自身的事件循环，并启动其管理的执行实体。
//
// Scheduler / WorkPool / ResultAggregator 是独立生命周期实体：
// Runtime 只负责启动与停止，不介入它们的内部调度与处理过程。
func (r *Runtime) Start() {
	if r.aggregator != nil {
		r.aggregator.Start()
	}
	if r.workPool != nil {
		r.workPool.Start()
	}
	if r.scheduler != nil {
		r.scheduler.Start()
	}
	go r.run()
}

// SetTools 注入工具注册表与 Checkpoint 存储（供 Supervisor 作为 ReAct Agent 使用）。
// 应在 Start 之前调用。
func (r *Runtime) SetTools(reg *agenttool.Registry, checkPoints adk.CheckPointStore) {
	r.tools = reg
	r.checkPoints = checkPoints
}

// SetExecution 注入受 Runtime 管理的三个独立执行实体及其存储。
//
// 依赖方向：Scheduler → (WorkPool, ResultAggregator) → ResearchState，
// ResultAggregator 通过 SchedulerNotifier 回调 Scheduler 完成回收确认。
// 三者都不反向依赖 SupervisorLoop。
//
// 应在 Start 之前调用。
func (r *Runtime) SetExecution(
	assignments storage.AssignmentStore,
	works storage.WorkStore,
	sched *scheduler.Scheduler,
	pool *workpool.WorkPool,
	agg *resultagg.Aggregator,
) {
	r.assignments = assignments
	r.works = works
	r.scheduler = sched
	r.workPool = pool
	r.aggregator = agg
}

// SetReporter 注入错误上报器（写日志 + 按 Severity 决定是否推送前端）。
//
// 应在 Start 之前调用；为 nil 时错误只写日志。
func (r *Runtime) SetReporter(reporter *fault.Reporter) {
	r.reporter = reporter
}

// reportError 上报一个错误；reporter 为 nil 时退化为只写日志。
//
// 只负责"上报"，不改变业务状态：状态变更仍由责任组件完成。
func (r *Runtime) reportError(err error, scope fault.Scope) {
	if err == nil {
		return
	}
	if r.reporter == nil {
		slog.Error("runtime: unreported error",
			"source", scope.Source, "task", scope.TaskID, "err", err)
		return
	}
	r.reporter.Report(context.Background(), err, scope)
}

// ExecutionStatus 返回执行实体的运行态摘要（诊断用）。
func (r *Runtime) ExecutionStatus() map[string]int {
	out := map[string]int{}
	if r.scheduler != nil {
		q, running := r.scheduler.Status()
		out["scheduler_queued"] = q
		out["scheduler_running"] = running
	}
	if r.workPool != nil {
		q, running := r.workPool.Status()
		out["workpool_queued"] = q
		out["workpool_running"] = running
	}
	if r.aggregator != nil {
		inbox, slots := r.aggregator.Status()
		out["aggregator_inbox"] = inbox
		out["aggregator_slots"] = slots
	}
	return out
}

// ProposeAssignments 由 SupervisorLoop 调用（源自模型调用的 submit_assignments 工具）：
// 把模型给出的研究目标落库并交给 Scheduler 调度。
//
// 是否派发、派发几个完全由模型决定，Runtime 不做轮次限流。
// Assignment 归属 RunTask：RunTask 被删除时其下 Assignment 一并删除。
//
// 返回 accepted 表示成功登记并交给 Scheduler 的数量。
func (r *Runtime) ProposeAssignments(ctx context.Context, taskID, runTaskID string, goals []string) (int, error) {
	if r.assignments == nil || r.bus == nil || len(goals) == 0 {
		return 0, nil
	}

	task, err := r.tasks.Get(ctx, taskID)
	if err != nil {
		return 0, err
	}
	now := r.now()
	list := make([]*domain.Assignment, 0, len(goals))
	for _, goal := range goals {
		if strings.TrimSpace(goal) == "" {
			continue
		}
		list = append(list, &domain.Assignment{
			ID:        ids.New(),
			SessionID: task.SessionID,
			TaskID:    taskID,
			RunTaskID: runTaskID,
			Goal:      goal,
			Status:    domain.AssignmentPending,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if len(list) == 0 {
		return 0, nil
	}

	slog.Info("runtime: proposing assignments", "task", taskID, "run_task", runTaskID, "count", len(list))
	// 同步发布，确保返回时 Scheduler 已完成登记与槽位预留。
	if err := r.bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID:   task.SessionID,
			TaskID:      taskID,
			RunTaskID:   runTaskID,
			Assignments: list,
		},
	}); err != nil {
		return 0, err
	}
	return len(list), nil
}

// CancelAssignments 由 SupervisorLoop 调用（源自模型调用的 cancel_assignments 工具）：
// 撤销一批已派发的 Assignment。
//
// 允许范围：研究结束（finish_research）之前，模型可随时撤销自己派发的子任务。
// 处理按 Assignment 的当前状态分别收尾（先标记状态，再按状态处置）：
//   - 刚入队   ：从调度队列移除；
//   - 正在执行 ：取消对应 Work；
//   - 等待提交 ：结果被提交门控拦下，不会进入 ResearchState；
//   - 已提交   ：从 ResearchState 中撤回（撤销同样是一次提交动作）。
//
// 返回成功撤销的数量，以及从 ResearchState 中撤回的已提交结果条数。
func (r *Runtime) CancelAssignments(ctx context.Context, taskID, runTaskID string, assignmentIDs []string, reason string) (int, int, error) {
	if r.scheduler == nil || len(assignmentIDs) == 0 {
		return 0, 0, nil
	}
	cancelled, revoked, err := r.scheduler.CancelAssignments(runTaskID, assignmentIDs, reason)
	if err != nil {
		return 0, 0, err
	}
	slog.Info("runtime: assignments cancelled",
		"task", taskID, "run_task", runTaskID,
		"count", cancelled, "revoked_results", revoked, "reason", reason)
	return cancelled, revoked, nil
}

// HasPendingAssignments 判断某个 RunTask 是否仍有未完成的 Assignment。
func (r *Runtime) HasPendingAssignments(ctx context.Context, runTaskID string) bool {
	if r.assignments == nil || runTaskID == "" {
		return false
	}
	list, err := r.assignments.ListByRunTask(ctx, runTaskID)
	if err != nil {
		return false
	}
	for _, a := range list {
		if !a.Status.IsTerminal() {
			return true
		}
	}
	return false
}

// Approve 提交一次工具审批决定，触发对应 Task 的 SupervisorLoop 走 Eino 恢复流程。
func (r *Runtime) Approve(_ context.Context, sessionID, interruptID string, approved bool, reason string) error {
	r.mu.Lock()
	var target *supervisor.Loop
	for _, l := range r.supervisors {
		if l.SessionID() != sessionID || !l.HasPendingApproval() {
			continue
		}
		target = l
		break
	}
	r.mu.Unlock()

	if target == nil {
		return supervisor.ErrNoPendingApproval
	}
	return target.Approve(interruptID, approved, reason)
}

// Send 实现 event.EventSender：异步投递（队列满时返回错误，作为背压信号）。
func (r *Runtime) Send(_ context.Context, e event.EventEnvelope) error {
	return r.enqueue(delivery{env: e})
}

// SendSync 实现 event.SyncEventSender：同步投递（入队后立即返回，
// 处理完成由事件循环回执）。
func (r *Runtime) SendSync(_ context.Context, e event.EventEnvelope, h *event.Handshake) error {
	return r.enqueue(delivery{env: e, h: h})
}

func (r *Runtime) enqueue(d delivery) error {
	select {
	case r.events <- d:
		return nil
	default:
		return errors.New("runtime: event queue full")
	}
}

// Shutdown 停止事件循环、所有 SupervisorLoop，以及 Runtime 管理的执行实体。
//
// 停止顺序与启动相反：先停调度入口（Scheduler），再停执行资源（WorkPool），
// 最后停结果处理（ResultAggregator），避免停止过程中仍有新 Work 涌入。
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.once.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		r.stopAllSupervisors()
		// 关闭失败只记录日志（Q6）：进程即将退出，前端可能已断开，
		// 且已提交数据不受影响；失败仅意味着可能残留 goroutine。
		if r.scheduler != nil {
			if err := r.scheduler.Shutdown(ctx); err != nil {
				slog.Warn("runtime: scheduler shutdown failed", "err", err)
			}
		}
		if r.workPool != nil {
			if err := r.workPool.Shutdown(ctx); err != nil {
				slog.Warn("runtime: workpool shutdown failed", "err", err)
			}
		}
		if r.aggregator != nil {
			if err := r.aggregator.Shutdown(ctx); err != nil {
				slog.Warn("runtime: aggregator shutdown failed", "err", err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) run() {
	defer close(r.done)
	for {
		select {
		case d := <-r.events:
			r.handle(d)
		case <-r.stop:
			return
		}
	}
}

func (r *Runtime) handle(d delivery) {
	err := r.process(context.Background(), d.env)
	if d.h == nil {
		if err != nil {
			slog.Error("runtime handle event failed",
				"type", d.env.Data.EventType(), "err", err)
		}
		return
	}
	// 第 2 次握手：回执（协商 + 原子落地的结果）。
	d.h.Reply(err)
	// 第 3 次握手：等待发布方确认。放到独立 goroutine，不阻塞事件循环，
	// 且带超时，避免发布方异常消失时永久挂起。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 确认回执失败只记录：发布方可能已取消等待，不影响状态落地。
		if err := d.h.Confirmed(ctx); err != nil {
			slog.Debug("runtime: handshake confirm failed", "err", err)
		}
	}()
}

func (r *Runtime) process(ctx context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.TaskEnabledEvent:
		return r.onTaskEnabled(ctx, ev.TaskID)
	case event.TaskDisabledEvent:
		return r.onTaskDisabled(ctx, ev.TaskID)
	case event.RunTaskDeleteRequestedEvent:
		return r.onRunTaskDelete(ctx, ev.TaskID)
	case event.StopRequestedEvent:
		return r.onStop(ctx, ev.TaskID)
	case event.ResearchFinishedEvent:
		return r.onResearchFinished(ctx, ev.TaskID)
	default:
		return nil
	}
}

// onTaskEnabled 处理 Task 使能：
//   - RunTask 不存在：协商创建，失能其他运行中的 Session，再新建 RunTask 并初始化研究；
//   - RunTask 已存在（Paused）：协商恢复，失能其他运行中的 Session，再置 Running。
func (r *Runtime) onTaskEnabled(ctx context.Context, taskID string) error {
	task, err := r.tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	sessionID := task.SessionID

	rt, err := r.runs.GetByTask(ctx, taskID)

	if errors.Is(err, storage.ErrNotFound) {
		// 使能时没有对应 RunTask：协商创建（初始化）后再新建。
		if err := r.coordinate(ctx, taskID, "", event.ActionCreate); err != nil {
			return err
		}
		// 全局单例运行：使能本 Session 前，先失能其他运行中的 Session。
		if err := r.disableOtherRunningSessions(ctx, taskID); err != nil {
			return err
		}
		now := r.now()
		rt = &domain.RunTask{
			ID:        ids.New(),
			TaskID:    taskID,
			Status:    domain.RunStatusRunning,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := r.runs.Create(ctx, rt); err != nil {
			return err
		}
		// 新建：没有"变更前状态"，previous 为空。
		r.publishRunStatusChanged(rt, "")
		return r.initResearchRun(ctx, taskID, rt.ID)
	}
	if err != nil {
		return err
	}

	// 已存在：协商恢复（继续）。仅 waiting（被挤掉）可恢复为 running。
	// 注意：Finished / Stopped 为终态，不可经"继续"复活。
	if rt.Status == domain.RunStatusFinished || rt.Status == domain.RunStatusStopped {
		if task.Enabled {
			// 保持终止态一致：Task 不应处于可运行标记。
			task.Enabled = false
			task.UpdatedAt = r.now()
			return r.tasks.Update(ctx, task)
		}
		return nil
	}

	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionResume); err != nil {
		return err
	}
	if err := r.disableOtherRunningSessions(ctx, taskID); err != nil {
		return err
	}
	previous := rt.Status
	rt.Status = domain.RunStatusRunning
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	r.publishRunStatusChanged(rt, previous)
	// 恢复研究：重新开启提交门控，并确保 SupervisorLoop 存在。
	r.states.ResumeCommits(ctx, taskID)
	r.ensureSupervisor(taskID, sessionID, rt.ID)
	return nil
}

// onTaskDisabled 处理 Task 失能：等价于用户中止，RunTask 置为 Stopped（终态）。
//
// 用户只有"中止"一个选择，因此不存在"暂停后继续"的语义：
// 失能后该 Task 不会因再次使能而复活，用户继续研究会在新 Task 上进行。
func (r *Runtime) onTaskDisabled(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // 无 RunTask，幂等
	}
	if err != nil {
		return err
	}
	if rt.Status != domain.RunStatusRunning {
		return nil // 已结束/已中止，无需处理
	}
	return r.globalStop(ctx, taskID, domain.RunStatusStopped)
}

// onStop 处理用户主动中止：执行全局中止，状态置为 Stopped（终止态）。
func (r *Runtime) onStop(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // 无 RunTask，幂等
	}
	if err != nil {
		return err
	}
	if rt.Status != domain.RunStatusRunning {
		return nil // 仅运行中可中止
	}
	return r.globalStop(ctx, taskID, domain.RunStatusStopped)
}

// onResearchFinished 处理模型主动宣布研究结束：Running -> Finished（终态）。
//
// 与"本轮结束"严格区分：本轮结束不改变 RunTask 状态，也不停止 SupervisorLoop；
// 只有模型调用 finish_research 才会走到这里。
//
// 终止时无论是否仍有 Assignment 在运行或等待提交，一律取消：
// 通过 ActionStop 协商，由 Scheduler / WorkPool / ResultAggregator 共同完成。
func (r *Runtime) onResearchFinished(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rt.Status != domain.RunStatusRunning {
		return nil // 非运行态，幂等
	}

	// 先取消全部后台任务（运行中 / 排队 / 等待提交），再终结研究。
	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionStop); err != nil {
		return err
	}
	r.terminateSupervisor(taskID)
	r.states.StopCommits(ctx, taskID)

	previous := rt.Status
	rt.Status = domain.RunStatusFinished
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	r.publishRunStatusChanged(rt, previous)
	// 研究完成：Task 不再处于可运行态。
	if task, err := r.tasks.Get(ctx, taskID); err == nil {
		task.Enabled = false
		task.UpdatedAt = r.now()
		// 状态落库失败重试一次；Task 已删除属正常（幂等）。
		err := fault.RetryOnce(ctx, func() error {
			if uErr := r.tasks.Update(ctx, task); uErr != nil && !fault.IsNotFound(uErr, storage.ErrNotFound) {
				return uErr
			}
			return nil
		})
		if err != nil {
			r.reportError(fault.E(fault.CodeInternal, fault.LayerRuntime,
				"failed to persist task disabled state").
				Severity(fault.SeverityError).
				Source("runtime.onResearchFinished").
				IDs(taskID, rt.ID, "", "").
				Wrap(err),
				fault.Scope{SessionID: rt.TaskID, TaskID: taskID, RunTaskID: rt.ID,
					Source: "runtime.onResearchFinished"})
		}
	}
	return nil
}

// onRunTaskDelete 协商后原子删除指定 Task 的 RunTask。
//
// Assignment 归属 RunTask：RunTask 消失则其下 Assignment（及其 Work）一并删除，
// 避免留下指向已删除 RunTask 的孤儿数据。
// 无 RunTask 时视为幂等（返回 nil）。
func (r *Runtime) onRunTaskDelete(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionDelete); err != nil {
		return err
	}
	if err := r.runs.Delete(ctx, rt.ID); err != nil {
		return err
	}
	r.stopSupervisor(taskID)
	// 级联删除该 RunTask 下的 Assignment（Work 由 Scheduler/WorkPool 侧同步清理）。
	if err := r.purgeRunTaskExecution(ctx, rt.ID); err != nil {
		slog.Warn("purge run task execution failed", "run_task", rt.ID, "err", err)
	}
	return nil
}

// purgeRunTaskExecution 删除某个 RunTask 下的全部 Assignment 与 Work。
//
// Assignment/Work 归属 RunTask：RunTask 消失后它们不再有意义，
// 必须一并删除，避免前端后台任务面板读到孤儿数据。
func (r *Runtime) purgeRunTaskExecution(ctx context.Context, runTaskID string) error {
	if runTaskID == "" {
		return nil
	}
	var firstErr error
	if r.assignments != nil {
		if err := r.assignments.DeleteByRunTask(ctx, runTaskID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r.works != nil {
		if err := r.works.DeleteByRunTask(ctx, runTaskID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// disableOtherRunningSessions 保证全局同一时间只有一个 RunTask 处于 Running。
//
// 被挤掉的 Session 置为 Waiting（不是 Paused）：Waiting 表示"因其他研究会话占用而暂挂"，
// Paused 只保留给用户主动暂停。
func (r *Runtime) disableOtherRunningSessions(ctx context.Context, currentTaskID string) error {
	runs, err := r.runs.List(ctx)
	if err != nil {
		return err
	}
	for _, rt := range runs {
		if rt.TaskID == currentTaskID {
			continue
		}
		if rt.Status != domain.RunStatusRunning {
			continue
		}
		if err := r.globalStop(ctx, rt.TaskID, domain.RunStatusWaiting); err != nil {
			return err
		}
	}
	return nil
}

// globalStop 协作式全局停止：协商后停止 SupervisorLoop、置 RunTask 为目标状态、失能 Task。
//
// 不同目标状态的处理差异（语义边界）：
//   - Running -> Waiting：被其他会话挤掉，保留 Checkpoint 与待审批项，等待"继续"；
//   - Running -> Paused ：用户主动暂停，同样保留现场，等待"继续"；
//   - Running -> Stopped：任务终结（终态），作废待审批项、删除 Checkpoint，
//     但保留 ResearchState 的全部内容（含 InterruptedContext）作为该节点的历史记录。
//
// 注意：Stopped 不关闭提交门控——属于本 Task 的迟到 Result 仍可正常提交。
func (r *Runtime) globalStop(ctx context.Context, taskID string, target domain.RunStatus) error {
	task, err := r.tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	rt, err := r.runs.GetByTask(ctx, taskID)
	if err != nil {
		return err
	}

	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionStop); err != nil {
		return err
	}

	if target == domain.RunStatusStopped {
		// 终态：作废待审批项并清理 Checkpoint，任务不再可恢复。
		r.terminateSupervisor(taskID)
	} else {
		// 暂停/等待：保留现场，仅停止运行。
		r.stopSupervisor(taskID)
		r.states.StopCommits(ctx, taskID)
	}

	// 置 RunTask 状态。
	previous := rt.Status
	rt.Status = target
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	r.publishRunStatusChanged(rt, previous)

	// 失能对应 Task。
	task.Enabled = false
	task.UpdatedAt = r.now()
	return r.tasks.Update(ctx, task)
}

// publishRunStatusChanged 发布 RunTask 状态变更事件（前端状态徽章的实时数据源）。
//
// previous 是变更前的状态；新建 RunTask 时传空。
// 只做通知：权威状态仍以 RunTaskStore 中的记录为准。
func (r *Runtime) publishRunStatusChanged(rt *domain.RunTask, previous domain.RunStatus) {
	if r.bus == nil || rt == nil {
		return
	}
	task, err := r.tasks.Get(context.Background(), rt.TaskID)
	if err != nil {
		slog.Warn("runtime: publish run status without task", "task", rt.TaskID, "err", err)
		return
	}
	_ = r.bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.RunStatusChangedEvent{
			SessionID: task.SessionID,
			TaskID:    rt.TaskID,
			RunTaskID: rt.ID,
			Status:    rt.Status,
			Previous:  previous,
		},
	})
	slog.Debug("runtime: run status changed",
		"task", rt.TaskID, "status", rt.Status, "previous", previous)
}

// terminateSupervisor 终止某个 Task 的 SupervisorLoop：作废待审批项、删除 Checkpoint、停止 Loop。
//
// 这是"中止"的专属路径，与 stopSupervisor（暂停/等待）区分开。
func (r *Runtime) terminateSupervisor(taskID string) {
	// 先取出 Loop，作废其待审批项，再删除对应 Checkpoint。
	r.mu.Lock()
	l, ok := r.supervisors[taskID]
	if ok {
		delete(r.supervisors, taskID)
	}
	r.mu.Unlock()

	if ok {
		l.CancelPendingApproval("任务已中止")
	}
	r.deleteCheckpoint(taskID)
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := l.Shutdown(ctx); err != nil {
			slog.Warn("runtime: supervisor loop shutdown failed", "task", taskID, "err", err)
		}
	}
}

// deleteCheckpoint 删除某个 Task 的 Checkpoint（若存储实现支持删除）。
func (r *Runtime) deleteCheckpoint(taskID string) {
	deleter, ok := r.checkPoints.(adk.CheckPointDeleter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := deleter.Delete(ctx, "task:"+taskID); err != nil {
		slog.Warn("delete checkpoint failed", "task", taskID, "err", err)
	}
}

// initResearchRun 在 RunTask 创建后初始化该 Task 的 ResearchState（按 TaskID），
// 若该 Session 已有上一份研究状态，则把其结论作为 RelatedFinding 继承，
// 并启动该 Task 的 SupervisorLoop。
//
// 注意：Task 的 Title/Description 只作为会话标识，不作为研究需求写入上下文；
// ResearchState 的 Requirement 由用户实际发送的第一条消息决定
// （见 states.SetRequirementIfEmpty）。
func (r *Runtime) initResearchRun(ctx context.Context, taskID, runTaskID string) error {
	task, err := r.tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	sessionID := task.SessionID

	// 把该 Session 下所有已存在的研究节点完整快照为前序节点（按序号升序），
	// 让新节点在上下文里能看到历史节点的需求、证据、结论与被中断原文。
	var related []researchstate.RelatedNode
	if prevs, err := r.states.ListBySession(ctx, sessionID); err == nil {
		for _, prev := range prevs {
			related = append(related, prev.Snapshot())
		}
	}

	// 序号 = 已有节点数 + 1；Requirements 初始为空，由用户消息按序填充。
	st := researchstate.NewWithRelated(ids.New(), taskID, sessionID, len(related)+1, related)
	if err := r.states.Create(ctx, st); err != nil {
		return err
	}
	r.ensureSupervisor(taskID, sessionID, runTaskID)
	return nil
}

// ensureSupervisor 确保指定 Task 的 SupervisorLoop 已创建并启动。
func (r *Runtime) ensureSupervisor(taskID, sessionID, runTaskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.supervisors[taskID]; ok {
		return
	}
	l := supervisor.New(supervisor.Config{
		Model:       r.model,
		States:      r.states,
		SessionID:   sessionID,
		TaskID:      taskID,
		RunTaskID:   runTaskID,
		Bus:         r.bus,
		Tools:       r.tools.Tools(),
		Policy:      r.tools.Policy(),
		CheckPoints: r.checkPoints,
		Proposer:    r,
		Canceller:   r,
		Assignments: r.assignments,
	})
	l.Start()
	r.supervisors[taskID] = l
}

// stopSupervisor 停止并移除指定 Task 的 SupervisorLoop。
func (r *Runtime) stopSupervisor(taskID string) {
	r.mu.Lock()
	l, ok := r.supervisors[taskID]
	if ok {
		delete(r.supervisors, taskID)
	}
	r.mu.Unlock()
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := l.Shutdown(ctx); err != nil {
			slog.Warn("runtime: supervisor loop shutdown failed", "task", taskID, "err", err)
		}
	}
}

func (r *Runtime) stopAllSupervisors() {
	r.mu.Lock()
	loops := make([]*supervisor.Loop, 0, len(r.supervisors))
	for _, l := range r.supervisors {
		loops = append(loops, l)
	}
	r.supervisors = make(map[string]*supervisor.Loop)
	r.mu.Unlock()

	for _, l := range loops {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := l.Shutdown(ctx); err != nil {
			slog.Warn("runtime: supervisor loop shutdown failed",
				"task_session", l.SessionID(), "err", err)
		}
		cancel()
	}
}

// coordinate 向依赖组件发起一次协商：发布 CoordinationRequested 事件并同步等待
// 所有依赖组件回执。任一组件拒绝或超时即失败，调用方不应继续落地状态。
// 创建场景下 runTaskID 为空（RunTask 尚未创建）。
func (r *Runtime) coordinate(ctx context.Context, taskID, runTaskID, action string) error {
	return r.bus.PublishSync(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "runtime",
			TaskID:     taskID,
			RunTaskID:  runTaskID,
			OccurredAt: r.now(),
		},
		Data: event.CoordinationRequestedEvent{TaskID: taskID, RunTaskID: runTaskID, Action: action},
	})
}
