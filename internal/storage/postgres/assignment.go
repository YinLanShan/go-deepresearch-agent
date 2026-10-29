package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// ─── AssignmentStore ────────────────────────────────────────────────────────

// AssignmentStore 是 Assignment 的 PostgreSQL 实现。
type AssignmentStore struct{ db *sql.DB }

// NewAssignmentStore 构造 Assignment 存储。
func NewAssignmentStore(db *sql.DB) *AssignmentStore { return &AssignmentStore{db: db} }

var _ storage.AssignmentStore = (*AssignmentStore)(nil)

const assignmentColumns = `id, session_id, task_id, run_task_id, goal, status,
	attempt, result, err, created_at, updated_at, finished_at`

// Create 保存一个 Assignment；ID 重复时返回 storage.ErrAlreadyExists。
func (s *AssignmentStore) Create(ctx context.Context, a *domain.Assignment) error {
	if a == nil || a.ID == "" {
		return storage.ErrInvalidArgument
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO assignments (`+assignmentColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		a.ID, a.SessionID, a.TaskID, a.RunTaskID, a.Goal, string(a.Status),
		a.Attempt, nullString(a.Result), nullString(a.Err),
		a.CreatedAt, a.UpdatedAt, nullTime(a.FinishedAt))
	return convertErr(err, fmt.Sprintf("assignment %q", a.ID))
}

// Get 按 ID 返回 Assignment；不存在时返回 storage.ErrNotFound。
func (s *AssignmentStore) Get(ctx context.Context, id string) (*domain.Assignment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+assignmentColumns+` FROM assignments WHERE id = $1`, id)
	return scanAssignment(row)
}

// ListByRunTask 返回某 RunTask 下的全部 Assignment，按创建时间升序。
func (s *AssignmentStore) ListByRunTask(ctx context.Context, runTaskID string) ([]*domain.Assignment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+assignmentColumns+` FROM assignments
		WHERE run_task_id = $1
		ORDER BY created_at ASC, id ASC`, runTaskID)
	if err != nil {
		return nil, convertErr(err, "")
	}
	defer rows.Close()

	out := make([]*domain.Assignment, 0)
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, convertErr(rows.Err(), "")
}

// Update 覆盖保存 Assignment；不存在时返回 storage.ErrNotFound。
func (s *AssignmentStore) Update(ctx context.Context, a *domain.Assignment) error {
	if a == nil || a.ID == "" {
		return storage.ErrInvalidArgument
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE assignments
		SET session_id = $2, task_id = $3, run_task_id = $4, goal = $5, status = $6,
		    attempt = $7, result = $8, err = $9, created_at = $10, updated_at = $11,
		    finished_at = $12
		WHERE id = $1`,
		a.ID, a.SessionID, a.TaskID, a.RunTaskID, a.Goal, string(a.Status),
		a.Attempt, nullString(a.Result), nullString(a.Err),
		a.CreatedAt, a.UpdatedAt, nullTime(a.FinishedAt))
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

// DeleteByRunTask 删除某 RunTask 下的全部 Assignment。
//
// 与内存实现一致：删除 0 行也返回 nil（幂等），因为这是级联清理而非业务删除。
// Work 由外键级联删除。
func (s *AssignmentStore) DeleteByRunTask(ctx context.Context, runTaskID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM assignments WHERE run_task_id = $1`, runTaskID)
	return convertErr(err, "")
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows，便于共用扫描逻辑。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAssignment 把一行扫描成 Assignment。
func scanAssignment(r rowScanner) (*domain.Assignment, error) {
	var (
		a          domain.Assignment
		status     string
		result     sql.NullString
		errText    sql.NullString
		finishedAt sql.NullTime
	)
	err := r.Scan(&a.ID, &a.SessionID, &a.TaskID, &a.RunTaskID, &a.Goal, &status,
		&a.Attempt, &result, &errText, &a.CreatedAt, &a.UpdatedAt, &finishedAt)
	if err != nil {
		return nil, convertErr(err, "")
	}
	a.Status = domain.AssignmentStatus(status)
	a.Result = result.String
	a.Err = errText.String
	a.FinishedAt = finishedAt.Time // 零值即 time.Time{}，与 NULL 语义对齐
	return &a, nil
}
