// Package service 是 Application Layer 的业务入口。
//
// RunService 负责 Task 启用/禁用、RunTask 查询/删除请求的编排，以及用户消息
// 与研究状态（ResearchState）的访问。RunTask 的创建与状态变更不由 RunService
// 直接完成，而是通过事件总线提交给 Runtime（独立生命周期实体），由 Runtime
// 协商依赖组件后原子落地。
package service

import (
	"context"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
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
//     （含初始化要求）再新建 RunTask；已存在则协商恢复为 running。
//   - 禁用：发布 EventTaskDisabled，Runtime 协商后暂停 RunTask 为 paused。
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

// SendUserMessage 向指定 Session 的研究提交一条用户消息（发布事件，异步处理）。
// 若 RunTask 已被中止，先恢复研究再提交消息。
func (s *RunService) SendUserMessage(ctx context.Context, sessionID, message string) error {
	if err := s.ensureRunning(ctx, sessionID); err != nil {
		return err
	}
	return s.bus.Publish(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			OccurredAt: s.now(),
		},
		Data: event.UserMessageEvent{SessionID: sessionID, Message: message},
	})
}

// RequestStop 发起用户主动中止请求（协作式、全局范围）。
func (s *RunService) RequestStop(ctx context.Context, sessionID string) error {
	return s.bus.PublishSync(ctx, event.EventEnvelope{
		Meta: event.EventMeta{
			EventID:    ids.New(),
			Source:     "run_service",
			OccurredAt: s.now(),
		},
		Data: event.StopRequestedEvent{SessionID: sessionID},
	})
}

// ensureRunning 若该 Session 的 RunTask 已中止，则先恢复研究。
func (s *RunService) ensureRunning(ctx context.Context, sessionID string) error {
	tasks, err := s.tasks.ListBySession(ctx, sessionID)
	if err != nil || len(tasks) == 0 {
		return nil
	}
	rt, err := s.runs.GetByTask(ctx, tasks[0].ID)
	if err != nil {
		return nil
	}
	if rt.Status == domain.RunStatusStopped {
		// 恢复：发布 TaskEnabled（Runtime 恢复门控 + SupervisorLoop + 状态）。
		return s.bus.PublishSync(ctx, event.EventEnvelope{
			Meta: event.EventMeta{
				EventID:    ids.New(),
				Source:     "run_service",
				TaskID:     tasks[0].ID,
				OccurredAt: s.now(),
			},
			Data: event.TaskEnabledEvent{TaskID: tasks[0].ID},
		})
	}
	return nil
}

// StartNewConversation 开启一个新对话：建立新的 ResearchState，并把上一对话的研究结论
// 写入 RelatedFinding（每新开展一次对话，即一个新的 ResearchState）。
func (s *RunService) StartNewConversation(ctx context.Context, sessionID, requirement string) (*researchstate.State, error) {
	var related []researchstate.RelatedFinding
	if prev, err := s.states.Get(ctx, sessionID); err == nil {
		related = append(related, researchstate.RelatedFinding{
			SessionID: prev.SessionID,
			Summary:   prev.Summary(),
		})
	}
	// 新对话使用新的 ResearchState ID，但仍绑定同一 Session。
	st := researchstate.NewWithRelated(ids.New(), sessionID, requirement, related)
	if err := s.states.Create(ctx, st); err != nil {
		return nil, err
	}
	return st, nil
}

// Bus 返回事件总线（供 API 层订阅流式输出事件等）。
func (s *RunService) Bus() *event.Bus { return s.bus }

// GetResearchState 查询指定 Session 的研究状态。
func (s *RunService) GetResearchState(ctx context.Context, sessionID string) (*researchstate.State, error) {
	return s.states.Get(ctx, sessionID)
}

// DeleteResearchState 删除指定 Session 的研究状态（Session 级联删除时调用）。
func (s *RunService) DeleteResearchState(ctx context.Context, sessionID string) error {
	return s.states.Delete(ctx, sessionID)
}
