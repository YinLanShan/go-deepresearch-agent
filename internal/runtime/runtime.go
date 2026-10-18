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
//   - RunTask 启动（创建）时，Runtime 初始化 ResearchState 与 SupervisorLoop。
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/supervisor"
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

	// supervisors 维护每个 Session 的 SupervisorLoop（RunTask 启动时创建）。
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
	}
	return r
}

// Start 启动事件处理 goroutine。
func (r *Runtime) Start() {
	go r.run()
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
		return r.onStop(ctx, ev.SessionID)
	default:
		return nil
	}
}

// onTaskEnabled 在 Task 启用时恢复对应 RunTask 为 running。
// 若 RunTask 不存在，先协商创建（含初始化要求），协商完成后新建 RunTask，
// 并初始化 ResearchState 与 SupervisorLoop。
func (r *Runtime) onTaskEnabled(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)

	// 使能时没有对应 RunTask：协商创建（初始化）后再新建。
	if errors.Is(err, storage.ErrNotFound) {
		if err := r.coordinate(ctx, taskID, "", event.ActionCreate); err != nil {
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
		return r.initResearchRun(ctx, taskID)
	}
	if err != nil {
		return err
	}

	// 已存在：协商恢复后置 running。
	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionResume); err != nil {
		return err
	}
	rt.Status = domain.RunStatusRunning
	rt.UpdatedAt = r.now()
	if err := r.runs.Update(ctx, rt); err != nil {
		return err
	}
	// 恢复研究：重新开启提交门控，并确保 SupervisorLoop 存在（中止后重建）。
	if task, err := r.tasks.Get(ctx, taskID); err == nil {
		_ = r.states.ResumeCommits(ctx, task.SessionID)
		r.ensureSupervisor(task.SessionID)
	}
	return nil
}

// onTaskDisabled 在 Task 禁用时，暂停对应 RunTask 为 paused。
// 无 RunTask 时视为幂等（返回 nil）。
func (r *Runtime) onTaskDisabled(ctx context.Context, taskID string) error {
	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionPause); err != nil {
		return err
	}
	rt.Status = domain.RunStatusPaused
	rt.UpdatedAt = r.now()
	return r.runs.Update(ctx, rt)
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
	// 停止该 RunTask 的 SupervisorLoop（ResearchState 属 Session，保留）。
	if task, err := r.tasks.Get(ctx, taskID); err == nil {
		r.stopSupervisor(task.SessionID)
	}
	return nil
}

// onStop 处理用户主动中止：协商后停止 SupervisorLoop、置 RunTask 为 stopped、
// 并关闭 ResearchState 的提交门控（停止接收新 Commit，正在进行的 Commit 继续完成）。
func (r *Runtime) onStop(ctx context.Context, sessionID string) error {
	tasks, err := r.tasks.ListBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil // 无 Task，无事可做
	}
	taskID := tasks[0].ID

	rt, err := r.runs.GetByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil // 无 RunTask，无事可做
	}
	if err != nil {
		return err
	}

	// 协作式中止：向依赖组件协商。
	if err := r.coordinate(ctx, taskID, rt.ID, event.ActionStop); err != nil {
		return err
	}

	// 停止 SupervisorLoop。
	r.stopSupervisor(sessionID)

	// 关闭 ResearchState 提交门控。
	_ = r.states.StopCommits(ctx, sessionID)

	// 置状态为 stopped。
	rt.Status = domain.RunStatusStopped
	rt.UpdatedAt = r.now()
	return r.runs.Update(ctx, rt)
}

// initResearchRun 在 RunTask 创建后初始化 ResearchState（按 Session，若不存在）
// 并启动该 Session 的 SupervisorLoop。
func (r *Runtime) initResearchRun(ctx context.Context, taskID string) error {
	task, err := r.tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	sessionID := task.SessionID
	if _, err := r.states.Get(ctx, sessionID); errors.Is(err, researchstate.ErrNotFound) {
		if err := r.states.Create(ctx, researchstate.New(ids.New(), sessionID, taskRequirement(task))); err != nil {
			return err
		}
	}
	r.ensureSupervisor(sessionID)
	return nil
}

// ensureSupervisor 确保该 Session 的 SupervisorLoop 已创建并启动。
func (r *Runtime) ensureSupervisor(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.supervisors[sessionID]; ok {
		return
	}
	l := supervisor.New(supervisor.Config{
		Model:     r.model,
		States:    r.states,
		SessionID: sessionID,
		Bus:       r.bus,
	})
	l.Start()
	r.supervisors[sessionID] = l
}

// stopSupervisor 停止并移除指定 Session 的 SupervisorLoop。
func (r *Runtime) stopSupervisor(sessionID string) {
	r.mu.Lock()
	l, ok := r.supervisors[sessionID]
	if ok {
		delete(r.supervisors, sessionID)
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

// taskRequirement 返回 Task 的第一条研究需求内容。
func taskRequirement(t *domain.Task) string {
	if t.Description != "" {
		return t.Description
	}
	return t.Title
}
