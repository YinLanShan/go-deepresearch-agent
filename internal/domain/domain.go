// Package domain 定义核心领域对象。
//
// 阶段 0 建立 Session、Task、RunTask 的基本结构，对应 docs/Architecture.md 5.1/5.2 节。
// Task 表示用户的研究意图（"研究什么"），不承载任何运行时状态；
// RunTask 表示"这个 Task 的一次具体执行"，是运行生命周期的管理对象；
// 真正的运行时资源（SupervisorLoop、WorkPool 等）在后续阶段引入。
package domain

import "time"

// Session 表示一次围绕研究目标展开的会话。
// 项目以 Session -> Task -> RunTask 作为推荐的组织关系。
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Task 表示用户提出的研究目标，是用户意图的表达，不直接代表一次运行。
//
// Enabled 表示 Task 是否允许启动新的执行（创建 RunTask）。
// 一个 Session 可对应多个 Task，但同一时间只能有一个 Task 处于运行态
// （全局也只有一个 RunTask 处于 Running）。Task 被禁用后禁止启动新的执行；
// 对已运行 RunTask 的处理由明确的生命周期策略决定，不与 Task 的启用/禁用行为默认绑定。
type Task struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	UserID      string    `json:"user_id,omitempty"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RunStatus 表示 RunTask 的生命周期状态。
//
// 状态语义：
//   - running  ：研究进行中（可能正在推理，也可能在等子任务结果）；
//   - waiting  ：被其他会话挤掉，可经"继续"切回；
//   - stopped  ：用户中止（终态，不可复活）；
//   - finished ：模型调用 finish_research 宣布研究结束（终态，不可复活）。
//
// 不存在"用户暂停"状态：用户只有"中止"这一个选择，中止即终态。
type RunStatus string

const (
	RunStatusCreated   RunStatus = "created"
	RunStatusRunning   RunStatus = "running"
	RunStatusStopped   RunStatus = "stopped"
	RunStatusFinished  RunStatus = "finished"
	RunStatusWaiting   RunStatus = "waiting"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

// RunTask 表示某个 Task 的一次具体执行，是运行生命周期的管理对象。
//
// RunTask 与 Task 为一对一关系：一个 Task 最多对应一个 RunTask。
// RunTask 承载本次运行的研究状态、Finding Graph、Assignment 状态、Checkpoint
// 及恢复信息（后续阶段逐步引入），其生命周期独立于 Task 的增删。
type RunTask struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	Status       RunStatus `json:"status"`
	Turn         int       `json:"turn"`
	StateVersion int64     `json:"state_version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
