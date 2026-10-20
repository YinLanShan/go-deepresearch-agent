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
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/supervisor"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
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

// Start 启动事件处理 goroutine。
func (r *Runtime) Start() {
	go r.run()
}

// SetTools 注入工具注册表与 Checkpoint 存储（供 Supervisor 作为 ReAct Agent 使用）。
// 应在 Start 之前调用。
func (r *Runtime) SetTools(reg *agenttool.Registry, checkPoints adk.CheckPointStore) {
	r.tools = reg
	r.checkPoints = checkPoints
}

// Approve 提交一次工具审批决定，触发对应 Task 的 SupervisorLoop 走 Eino 恢复流程。
func (r *Runtime) Approve(_ context.Context, sessionID, interruptID string, approved bool, reason string) error {
	r.mu.Lock()
	var target *supervisor.Loop
	for taskID, l := range r.supervisors {
		_ = taskID
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

// Shutdown 停止事件循环并等待退出，并停止所有 SupervisorLoop。
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.once.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		r.stopAllSupervisors()
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
		_ = d.h.Confirmed(ctx)
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
		return r.initResearchRun(ctx, taskID, rt.ID)
	}
	if err != nil {
		return err
	}

	// 已存在：协商恢复（继续），由 Paused 置 Running。
	// 注意：Finished / Stopped 为终止态，不可经"继续"复活；仅 Paused（及 Created）可恢复。
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
	rt.Status = domain.RunStatusRunning
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	// 恢复研究：重新开启提交门控，并确保 SupervisorLoop 存在。
	_ = r.states.ResumeCommits(ctx, taskID)
	r.ensureSupervisor(taskID, sessionID, rt.ID)
	return nil
}

// onTaskDisabled 处理 Task 失能：执行全局中止，但状态置为 Paused（可经"继续"恢复）。
func (r *Runtime) onTaskDisabled(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // 无 RunTask，幂等
	}
	if err != nil {
		return err
	}
	if rt.Status != domain.RunStatusRunning {
		return nil // 已暂停/已完成/已中止，无需处理
	}
	return r.globalStop(ctx, taskID, domain.RunStatusPaused)
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

// onResearchFinished 处理模型完成研究：Running -> Finished（终止态）。
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
	r.stopSupervisor(taskID)
	_ = r.states.StopCommits(ctx, taskID)
	rt.Status = domain.RunStatusFinished
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	// 研究完成：Task 不再处于可运行态。
	if task, err := r.tasks.Get(ctx, taskID); err == nil {
		task.Enabled = false
		task.UpdatedAt = r.now()
		_ = r.tasks.Update(ctx, task)
	}
	return nil
}

// onRunTaskDelete 协商后原子删除指定 Task 的 RunTask。
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
	return nil
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
		_ = r.states.StopCommits(ctx, taskID)
	}

	// 置 RunTask 状态。
	rt.Status = target
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}

	// 失能对应 Task。
	task.Enabled = false
	task.UpdatedAt = r.now()
	return r.tasks.Update(ctx, task)
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
		_ = l.Shutdown(ctx)
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

	// 序号 = 已有节点数 + 1；需求初始为空，由首条用户消息填充。
	st := researchstate.NewWithRelated(ids.New(), taskID, sessionID, len(related)+1, "", related)
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
		_ = l.Shutdown(ctx)
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
		_ = l.Shutdown(ctx)
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
