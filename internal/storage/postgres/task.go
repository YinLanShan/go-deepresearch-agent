package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// ─── TaskStore ──────────────────────────────────────────────────────────────

// TaskStore 是 Task 的 PostgreSQL 实现。
type TaskStore struct{ db *sql.DB }

// NewTaskStore 构造 Task 存储。
func NewTaskStore(db *sql.DB) *TaskStore { return &TaskStore{db: db} }

var _ storage.TaskStore = (*TaskStore)(nil)

// Create 保存一个 Task；ID 重复时返回 storage.ErrAlreadyExists。
func (s *TaskStore) Create(ctx context.Context, t *domain.Task) error {
	if t == nil || t.ID == "" {
		return storage.ErrInvalidArgument
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (id, session_id, user_id, title, description, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.SessionID, t.UserID, t.Title, nullString(t.Description), t.Enabled, t.CreatedAt, t.UpdatedAt)
	return convertErr(err, fmt.Sprintf("task %q", t.ID))
}

// Get 按 ID 返回 Task；不存在时返回 storage.ErrNotFound。
func (s *TaskStore) Get(ctx context.Context, id string) (*domain.Task, error) {
	var (
		t     domain.Task
		descr sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, user_id, title, description, enabled, created_at, updated_at
		FROM tasks WHERE id = $1`, id).
		Scan(&t.ID, &t.SessionID, &t.UserID, &t.Title, &descr, &t.Enabled, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, convertErr(err, "")
	}
	t.Description = descr.String
	return &t, nil
}

// ListBySession 返回某 Session 的全部 Task，按创建时间升序。
func (s *TaskStore) ListBySession(ctx context.Context, sessionID string) ([]*domain.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, user_id, title, description, enabled, created_at, updated_at
		FROM tasks WHERE session_id = $1
		ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, convertErr(err, "")
	}
	defer rows.Close()

	out := make([]*domain.Task, 0)
	for rows.Next() {
		var (
			t     domain.Task
			descr sql.NullString
		)
		if err := rows.Scan(&t.ID, &t.SessionID, &t.UserID, &t.Title, &descr,
			&t.Enabled, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, convertErr(err, "")
		}
		t.Description = descr.String
		out = append(out, &t)
	}
	return out, convertErr(rows.Err(), "")
}

// Update 覆盖保存 Task；不存在时返回 storage.ErrNotFound。
func (s *TaskStore) Update(ctx context.Context, t *domain.Task) error {
	if t == nil || t.ID == "" {
		return storage.ErrInvalidArgument
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET session_id = $2, user_id = $3, title = $4, description = $5,
		    enabled = $6, updated_at = $7
		WHERE id = $1`,
		t.ID, t.SessionID, t.UserID, t.Title, nullString(t.Description), t.Enabled, t.UpdatedAt)
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

// Delete 删除 Task；不存在时返回 storage.ErrNotFound。
//
// RunTask 与 ResearchState 由外键级联删除。
func (s *TaskStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, id)
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

// nullString 把空字符串转成 NULL，避免与"未设置"混淆。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
