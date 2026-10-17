// Package service 是 Application Layer 的业务入口。
//
// RunService 负责 Task 启用/禁用与 RunTask 查询/删除请求的编排。RunTask 的
// 创建与状态变更不由 RunService 直接完成，而是通过事件总线提交给 Runtime
// （独立生命周期实体），由 Runtime 协商依赖组件后原子落地。
package service

import (
	"context"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// RunService 负责 Task 的启用/禁用与 RunTask 的查询/删除请求编排。
type RunService struct {
	tasks storage.TaskStore
	runs  storage.RunTaskStore
	bus   *event.Bus
	now   func() time.Time
}

// NewRunService 构造一个 RunService。
func NewRunService(tasks storage.TaskStore, runs storage.RunTaskStore, bus *event.Bus) *RunService {
	return &RunService{tasks: tasks, runs: runs, bus: bus, now: time.Now}
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
