package domain

import "time"

// AssignmentStatus 是 Assignment 的生命周期状态。
type AssignmentStatus string

const (
	// AssignmentPending 表示已提交但尚未被 Scheduler 调度。
	AssignmentPending AssignmentStatus = "pending"
	// AssignmentScheduled 表示已排入调度队列、等待 Worker。
	AssignmentScheduled AssignmentStatus = "scheduled"
	// AssignmentRunning 表示已分配给 Worker 正在执行。
	AssignmentRunning AssignmentStatus = "running"
	// AssignmentCompleted 表示结果已被 ResultAggregator 成功提交。
	AssignmentCompleted AssignmentStatus = "completed"
	// AssignmentFailed 表示执行或提交失败。
	AssignmentFailed AssignmentStatus = "failed"
	// AssignmentCancelled 表示被取消（含全局中止）。
	AssignmentCancelled AssignmentStatus = "cancelled"

	// AssignmentAwaitingResult 表示 Work 已结束但结果尚未提交。
	AssignmentAwaitingResult AssignmentStatus = "awaiting_result"
)

// IsTerminal 判断该状态是否为终态。
func (s AssignmentStatus) IsTerminal() bool {
	switch s {
	case AssignmentCompleted, AssignmentFailed, AssignmentCancelled:
		return true
	default:
		return false
	}
}

// Assignment 表示"需要完成什么工作"。
//
// 边界（对应 docs/Architecture.md 5.4/5.5）：
//   - Assignment = What to do，由 Supervisor 决定并提交；
//   - Work = One execution of that work，由 Scheduler 调度后产生；
//   - Assignment 不直接代表运行资源，其执行实例是 Work。
type Assignment struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	RunTaskID string `json:"run_task_id"`

	// Goal 是要完成的研究目标（自然语言）。
	Goal string `json:"goal"`

	Status AssignmentStatus `json:"status"`

	// Attempt 是该 Assignment 已尝试的次数（重试时递增）。
	Attempt int `json:"attempt"`

	// Result 是最终提交的研究产物（由 ResultAggregator 写入）。
	Result string `json:"result,omitempty"`
	// Err 记录失败原因。
	Err string `json:"err,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// WorkStatus 是 Work 的生命周期状态。
type WorkStatus string

const (
	// WorkPending 表示已创建、尚未分配到 Worker。
	WorkPending WorkStatus = "pending"
	// WorkRunning 表示正在执行。
	WorkRunning WorkStatus = "running"
	// WorkSucceeded 表示执行成功并产出结果。
	WorkSucceeded WorkStatus = "succeeded"
	// WorkFailed 表示执行失败。
	WorkFailed WorkStatus = "failed"
	// WorkCancelled 表示被取消（全局中止）。
	WorkCancelled WorkStatus = "cancelled"
	// WorkTimedOut 表示超时（当前阶段不实现真实超时中断，仅保留状态）。
	WorkTimedOut WorkStatus = "timedout"
)

// IsTerminal 判断该状态是否为终态。
func (s WorkStatus) IsTerminal() bool {
	switch s {
	case WorkSucceeded, WorkFailed, WorkCancelled, WorkTimedOut:
		return true
	default:
		return false
	}
}

// WorkConstraint 描述 Work 的执行约束。
type WorkConstraint struct {
	// ToolAllowed 允许该 Work 使用的工具（暂不实现）。
	ToolAllowed []string `json:"tool_allowed,omitempty"`
	// TimeLimit 是执行时限；当前阶段不实现真实超时中断。
	TimeLimit time.Duration `json:"time_limit,omitempty"`
}

// Work 表示 Assignment 的一次实际执行实例。
//
// 一个 Assignment 可以对应多个 Work（重试）；每个 Work 由某个 Worker 执行。
// Work 是纯数据对象，不内嵌锁：并发控制由 WorkStore 与 WorkPool 负责。
type Work struct {
	ID           string `json:"id"`
	AssignmentID string `json:"assignment_id"`
	SessionID    string `json:"session_id"`
	TaskID       string `json:"task_id"`
	RunTaskID    string `json:"run_task_id"`

	// Goal 冗余保存 Assignment 的目标，便于前端直接展示子 Agent 任务。
	Goal string `json:"goal"`

	Status     WorkStatus      `json:"status"`
	Constraint *WorkConstraint `json:"constraint,omitempty"`

	// WorkerID 是执行该 Work 的 Worker 标识。
	WorkerID string `json:"worker_id,omitempty"`

	// Attempt 是第几次尝试（从 1 开始）。
	Attempt int `json:"attempt"`

	// Stage 是当前执行阶段（用于前端展示实时进度）。
	Stage string `json:"stage,omitempty"`
	// Detail 是当前阶段的补充描述。
	Detail string `json:"detail,omitempty"`

	// Result 是产出的研究产物（成功后写入）。
	Result string `json:"result,omitempty"`
	// Err 是失败原因。
	Err string `json:"err,omitempty"`

	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
