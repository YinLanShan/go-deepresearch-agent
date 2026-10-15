// Package domain 定义核心领域对象。
//
// 阶段 0 仅建立 Session 与 Task 的基本结构，对应 docs/Architecture.md 5.1 节。
// Task 表示用户的研究意图（"研究什么"），不承载任何运行时状态；
// RunTask、ResearchState 等运行时对象在后续阶段引入。
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
type Task struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	UserID      string    `json:"user_id,omitempty"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
