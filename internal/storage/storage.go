// Package storage 定义研究数据存储的抽象接口。
//
// 对应 docs/Architecture.md 3.3 / 5.17 节：核心业务模块只依赖抽象，
// 不直接依赖具体数据库。阶段 0 提供接口定义与内存实现，
// 后续可替换为 SQLite / PostgreSQL 等实现而不改动调用方。
package storage

import (
	"context"
	"errors"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// ErrNotFound 表示目标对象不存在。
var ErrNotFound = errors.New("storage: not found")

// ErrSessionHasTask 表示 Session 已关联 Task，
// 违反"一个 Session 只对应一个 Task"的唯一性约束。
var ErrSessionHasTask = errors.New("storage: session already has a task")

// ErrRunTaskExists 表示 Task 已关联 RunTask，
// 违反"一个 Task 最多对应一个 RunTask"的唯一性约束。
var ErrRunTaskExists = errors.New("storage: run task already exists for task")

// SessionStore 定义 Session 的持久化能力。
type SessionStore interface {
	Create(ctx context.Context, s *domain.Session) error
	Get(ctx context.Context, id string) (*domain.Session, error)
	List(ctx context.Context) ([]*domain.Session, error)
	Delete(ctx context.Context, id string) error
}

// TaskStore 定义 Task 的持久化能力。
//
// Create 必须保证"一个 Session 只对应一个 Task"：当目标 Session
// 已存在 Task 时，返回 ErrSessionHasTask。所有实现（含未来持久化实现）
// 都必须在同一写入路径内原子地保证该唯一性，避免并发重复创建。
type TaskStore interface {
	Create(ctx context.Context, t *domain.Task) error
	Get(ctx context.Context, id string) (*domain.Task, error)
	ListBySession(ctx context.Context, sessionID string) ([]*domain.Task, error)
	Update(ctx context.Context, t *domain.Task) error
	Delete(ctx context.Context, id string) error
}

// RunTaskStore 定义 RunTask 的持久化能力。
//
// Create 必须保证"一个 Task 最多对应一个 RunTask"：当目标 Task
// 已存在 RunTask 时，返回 ErrRunTaskExists。所有实现（含未来持久化实现）
// 都必须在同一写入路径内原子地保证该唯一性，避免并发重复创建。
//
// RunTask 的生命周期由 Runtime 独立维护；Task 的启用/禁用通过事件驱动
// RunTask 的创建与状态变更，不直接由 HTTP Handler 创建。
type RunTaskStore interface {
	Create(ctx context.Context, rt *domain.RunTask) error
	Get(ctx context.Context, id string) (*domain.RunTask, error)
	GetByTask(ctx context.Context, taskID string) (*domain.RunTask, error)
	Update(ctx context.Context, rt *domain.RunTask) error
	Delete(ctx context.Context, id string) error
}
