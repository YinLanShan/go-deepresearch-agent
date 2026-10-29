package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// ─── RunTaskStore ───────────────────────────────────────────────────────────

// RunTaskStore 是 RunTask 的 PostgreSQL 实现。
type RunTaskStore struct{ db *sql.DB }

// NewRunTaskStore 构造 RunTask 存储。
func NewRunTaskStore(db *sql.DB) *RunTaskStore { return &RunTaskStore{db: db} }

var _ storage.RunTaskStore = (*RunTaskStore)(nil)

// Create 保存一个 RunTask。
//
// 错误语义（与内存实现一致）：
//   - 参数为空 → storage.ErrInvalidArgument；
//   - ID 已存在 → storage.ErrAlreadyExists；
//   - 该 Task 已关联其他 RunTask → storage.ErrRunTaskExists（唯一约束 uk 触发）。
func (s *RunTaskStore) Create(ctx context.Context, rt *domain.RunTask) error {
	if rt == nil || rt.ID == "" || rt.TaskID == "" {
		return storage.ErrInvalidArgument
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_tasks (id, task_id, status, turn, state_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rt.ID, rt.TaskID, string(rt.Status), rt.Turn, rt.StateVersion, rt.CreatedAt, rt.UpdatedAt)

	// 唯一冲突有两种来源：主键重复（ID 已存在）与 task_id 唯一（已有别的 RunTask）。
	// 用"该 ID 是否已存在"来区分，给出与内存实现一致的哨兵错误。
	if isUniqueViolation(err) {
		if existing, getErr := s.Get(ctx, rt.ID); getErr == nil && existing != nil {
			return fmt.Errorf("%w: run task %q", storage.ErrAlreadyExists, rt.ID)
		}
		return fmt.Errorf("%w: task %q", storage.ErrRunTaskExists, rt.TaskID)
	}
	return convertErr(err, "")
}

// Get 按 ID 返回 RunTask；不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) Get(ctx context.Context, id string) (*domain.RunTask, error) {
	return s.queryOne(ctx, `
		SELECT id, task_id, status, turn, state_version, created_at, updated_at
		FROM run_tasks WHERE id = $1`, id)
}

// GetByTask 按 TaskID 返回 RunTask；不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) GetByTask(ctx context.Context, taskID string) (*domain.RunTask, error) {
	return s.queryOne(ctx, `
		SELECT id, task_id, status, turn, state_version, created_at, updated_at
		FROM run_tasks WHERE task_id = $1`, taskID)
}

// List 返回全部 RunTask，按创建时间升序。
func (s *RunTaskStore) List(ctx context.Context) ([]*domain.RunTask, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, status, turn, state_version, created_at, updated_at
		FROM run_tasks ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, convertErr(err, "")
	}
	defer rows.Close()

	out := make([]*domain.RunTask, 0)
	for rows.Next() {
		var (
			rt     domain.RunTask
			status string
		)
		if err := rows.Scan(&rt.ID, &rt.TaskID, &status, &rt.Turn,
			&rt.StateVersion, &rt.CreatedAt, &rt.UpdatedAt); err != nil {
			return nil, convertErr(err, "")
		}
		rt.Status = domain.RunStatus(status)
		out = append(out, &rt)
	}
	return out, convertErr(rows.Err(), "")
}

// Update 覆盖保存 RunTask；不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) Update(ctx context.Context, rt *domain.RunTask) error {
	if rt == nil || rt.ID == "" {
		return storage.ErrInvalidArgument
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE run_tasks
		SET task_id = $2, status = $3, turn = $4, state_version = $5, updated_at = $6
		WHERE id = $1`,
		rt.ID, rt.TaskID, string(rt.Status), rt.Turn, rt.StateVersion, rt.UpdatedAt)
	if err != nil {
		return convertErr(err, "")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// Delete 删除 RunTask；不存在时返回 storage.ErrNotFound。
//
// Assignment 与 Work 由外键级联删除。
func (s *RunTaskStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM run_tasks WHERE id = $1`, id)
	if err != nil {
		return convertErr(err, "")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// queryOne 是 Get / GetByTask 的共用实现。
func (s *RunTaskStore) queryOne(ctx context.Context, query string, args ...any) (*domain.RunTask, error) {
	var (
		rt     domain.RunTask
		status string
	)
	err := s.db.QueryRowContext(ctx, query, args...).
		Scan(&rt.ID, &rt.TaskID, &status, &rt.Turn, &rt.StateVersion, &rt.CreatedAt, &rt.UpdatedAt)
	if err != nil {
		return nil, convertErr(err, "")
	}
	rt.Status = domain.RunStatus(status)
	return &rt, nil
}
