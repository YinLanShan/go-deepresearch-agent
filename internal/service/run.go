// Package service 是 Application Layer 的业务入口。
//
// RunService 负责 Task 启用/禁用、RunTask 查询/删除请求的编排，以及用户消息
// 与研究状态（ResearchState）的访问。RunTask 的创建与状态变更不由 RunService
// 直接完成，而是通过事件总线提交给 Runtime（独立生命周期实体），由 Runtime
// 协商依赖组件后原子落地。
package service

import (
	"context"
	"errors"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// RunService 负责 Task 的启用/禁用、RunTask 的查询/删除请求，以及研究状态访问。
type RunService struct {
	tasks  storage.TaskStore
	runs   storage.RunTaskStore
	states researchstate.Store
	bus    *event.Bus
	now    func() time.Time
}

// NewRunService 构造一个 RunService。
func NewRunService(tasks storage.TaskStore, runs storage.RunTaskStore, states researchstate.Store,
	bus *event.Bus) *RunService {
	return &RunService{tasks: tasks, runs: runs, states: states, bus: bus, now: time.Now}
}

// SetTaskEnabled 修改 Task 的启用/禁用状态，并以同步事件提交给 Runtime 协商处理。
//
//   - 启用：发布 EventTaskEnabled。Runtime 若发现 RunTask 不存在，会先协商创建
//     （含初始化要求）再新建 RunTask；已存在（Paused）则协商恢复为 running。
//   - 禁用：发布 EventTaskDisabled，Runtime 执行全局中止并把 RunTask 置为 paused。
//
// 不在此做 RunTask 存在性预检：RunTask 的创建由 Runtime 协商完成。
// 状态未变化时幂等返回，不发布事件。
func (s *RunService) SetTaskEnabled(ctx context.Context, taskID string, enabled bool) (*domain.Task, error) {
	task, err := s.tasks.Get(ctx, taskID)
	if err != nil {
		return nil, err // storage.ErrNotFound 等
	}
	if task.Enabled == enabled {
		return task, nil
	}

	task.Enabled = enabled
	task.UpdatedAt = s.now()
	if err := s.tasks.Update(ctx, task); err != nil {
		return nil, err
	}

	var ev event.Event
	if enabled {
		ev = event.TaskEnabledEvent{TaskID: taskID}
	} else {
		ev = event.TaskDisabledEvent{TaskID: taskID}
	}
	if err := s.bus.PublishSync(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			TaskID:     taskID,
			OccurredAt: s.now(),
		},
		Data: ev,
	}); err != nil {
		return nil, err
	}
	return task, nil
}

// GetRunTask 按 RunTaskID 查询。
func (s *RunService) GetRunTask(ctx context.Context, runTaskID string) (*domain.RunTask, error) {
	return s.runs.Get(ctx, runTaskID)
}

// GetRunTaskByTask 查询指定 Task 对应的 RunTask。
func (s *RunService) GetRunTaskByTask(ctx context.Context, taskID string) (*domain.RunTask, error) {
	return s.runs.GetByTask(ctx, taskID)
}

// RequestDeleteRunTask 发起删除指定 Task 的 RunTask 的请求。
//
// 删除不由本方法直接操作存储，而是发布 EventRunTaskDeleteRequested 事件，
// 由 Runtime 协商依赖组件（协作式结束生命周期）后原子删除。
// 用于 Session 删除时的级联清理。
func (s *RunService) RequestDeleteRunTask(ctx context.Context, taskID string) error {
	return s.bus.PublishSync(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			TaskID:     taskID,
			OccurredAt: s.now(),
		},
		Data: event.RunTaskDeleteRequestedEvent{TaskID: taskID},
	})
}

// SendUserMessage 向指定 Session 的研究提交一条用户消息。
//
// 语义（对应研究生命周期）：
//   - 当前任务 Running：直接投递给其 SupervisorLoop；
//   - 当前任务 Finished / Stopped（终止态）：新建一个 Task（研究需求=本条消息），
//     新建一份 ResearchState（继承上一份的结论为 RelatedFinding）并启用，再投递消息；
//   - 当前任务 Paused：需先经"继续"恢复，返回错误。
func (s *RunService) SendUserMessage(ctx context.Context, sessionID, message string) error {
	task, rt, err := s.currentResearch(ctx, sessionID)
	if err != nil {
		return err
	}
	if task == nil {
		// 无 Task：直接发布（无 SupervisorLoop，消息会被丢弃）。正常流程应先创建并启用。
		return s.publishMessage(ctx, sessionID, "", "", message)
	}
	if rt == nil {
		// 有 Task 但尚未启用：直接发布（无 SupervisorLoop）。
		return s.publishMessage(ctx, sessionID, task.ID, "", message)
	}

	switch rt.Status {
	case domain.RunStatusFinished, domain.RunStatusStopped:
		return s.startNextResearch(ctx, sessionID, message)
	case domain.RunStatusPaused:
		return fault.New(fault.CodeInvalidInput, fault.LayerRuntime,
			"research is paused; continue it first")
	default:
		return s.publishMessage(ctx, sessionID, task.ID, rt.ID, message)
	}
}

// RequestStop 发起用户主动中止请求（协作式、全局范围），作用于当前运行中的任务。
func (s *RunService) RequestStop(ctx context.Context, sessionID string) error {
	task, rt, err := s.currentResearch(ctx, sessionID)
	if err != nil {
		return err
	}
	if task == nil || rt == nil || rt.Status != domain.RunStatusRunning {
		return nil // 无运行中任务，幂等
	}
	return s.bus.PublishSync(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			TaskID:     task.ID,
			RunTaskID:  rt.ID,
			OccurredAt: s.now(),
		},
		Data: event.StopRequestedEvent{SessionID: sessionID, TaskID: task.ID, RunTaskID: rt.ID},
	})
}

// currentResearch 返回指定 Session 当前（最近创建的）Task 及其 RunTask。
// 无 Task 时返回 (nil, nil, nil)。
func (s *RunService) currentResearch(ctx context.Context, sessionID string) (*domain.Task, *domain.RunTask, error) {
	tasks, err := s.tasks.ListBySession(ctx, sessionID)
	if err != nil || len(tasks) == 0 {
		return nil, nil, err
	}
	task := tasks[len(tasks)-1] // ListBySession 按创建时间升序，最后一个为最新
	rt, err := s.runs.GetByTask(ctx, task.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return task, nil, nil
		}
		return nil, nil, err
	}
	return task, rt, nil
}

// startNextResearch 在上一任务终止（Finished/Stopped）后开启新研究：
// 新建 Task（研究需求=本条消息）→ 启用（Runtime 创建 RunTask 与新 ResearchState，
// 并继承上一份研究状态的结论为 RelatedFinding）→ 投递消息。
func (s *RunService) startNextResearch(ctx context.Context, sessionID, message string) error {
	now := s.now()
	t := &domain.Task{
		ID:          ids.New(),
		SessionID:   sessionID,
		Title:       "继续研究",
		Description: message,
		Enabled:     false,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.tasks.Create(ctx, t); err != nil {
		return err
	}
	if _, err := s.SetTaskEnabled(ctx, t.ID, true); err != nil {
		return err
	}
	rt, err := s.runs.GetByTask(ctx, t.ID)
	if err != nil {
		return err
	}
	return s.publishMessage(ctx, sessionID, t.ID, rt.ID, message)
}

// publishMessage 发布用户消息事件。
func (s *RunService) publishMessage(ctx context.Context, sessionID, taskID, runTaskID, message string) error {
	return s.bus.Publish(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			TaskID:     taskID,
			RunTaskID:  runTaskID,
			OccurredAt: s.now(),
		},
		Data: event.UserMessageEvent{SessionID: sessionID, TaskID: taskID, RunTaskID: runTaskID, Message: message},
	})
}

// Bus 返回事件总线（供 API 层订阅流式输出事件等）。
func (s *RunService) Bus() *event.Bus { return s.bus }

// GetResearchState 查询指定 Session 当前研究任务（最近一份）的研究状态。
func (s *RunService) GetResearchState(ctx context.Context, sessionID string) (*researchstate.State, error) {
	return s.states.GetLatestBySession(ctx, sessionID)
}

// DeleteResearchState 删除指定 Session 的全部研究状态（Session 级联删除时调用）。
func (s *RunService) DeleteResearchState(ctx context.Context, sessionID string) error {
	return s.states.DeleteBySession(ctx, sessionID)
}
