package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// ─── WorkStore ──────────────────────────────────────────────────────────────

// WorkStore 是 Work 的 PostgreSQL 实现。
type WorkStore struct{ db *sql.DB }

// NewWorkStore 构造 Work 存储。
func NewWorkStore(db *sql.DB) *WorkStore { return &WorkStore{db: db} }

var _ storage.WorkStore = (*WorkStore)(nil)

const workColumns = `id, assignment_id, session_id, task_id, run_task_id, goal,
	status, constraint_data, worker_id, attempt, stage, detail, result, err,
	started_at, finished_at, created_at, updated_at`

// Create 保存一个 Work；ID 重复时返回 storage.ErrAlreadyExists。
func (s *WorkStore) Create(ctx context.Context, w *domain.Work) error {
	if w == nil || w.ID == "" {
		return storage.ErrInvalidArgument
	}
	constraint, err := marshalConstraint(w.Constraint)
	if err != nil {
		return err
	}
	_, execErr := s.db.ExecContext(ctx, `
		INSERT INTO works (`+workColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		w.ID, w.AssignmentID, w.SessionID, w.TaskID, w.RunTaskID, w.Goal,
		string(w.Status), constraint, w.WorkerID, w.Attempt, w.Stage, w.Detail,
		nullString(w.Result), nullString(w.Err),
		nullTime(w.StartedAt), nullTime(w.FinishedAt), w.CreatedAt, w.UpdatedAt)
	return convertErr(execErr, fmt.Sprintf("work %q", w.ID))
}

// Get 按 ID 返回 Work；不存在时返回 storage.ErrNotFound。
func (s *WorkStore) Get(ctx context.Context, id string) (*domain.Work, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workColumns+` FROM works WHERE id = $1`, id)
	return scanWork(row)
}

// ListByRunTask 返回某 RunTask 下的全部 Work，按创建时间升序。
func (s *WorkStore) ListByRunTask(ctx context.Context, runTaskID string) ([]*domain.Work, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+workColumns+` FROM works
		WHERE run_task_id = $1
		ORDER BY created_at ASC, id ASC`, runTaskID)
	if err != nil {
		return nil, convertErr(err, "")
	}
	defer rows.Close()

	out := make([]*domain.Work, 0)
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, convertErr(rows.Err(), "")
}

// Update 覆盖保存 Work；不存在时返回 storage.ErrNotFound。
func (s *WorkStore) Update(ctx context.Context, w *domain.Work) error {
	if w == nil || w.ID == "" {
		return storage.ErrInvalidArgument
	}
	constraint, err := marshalConstraint(w.Constraint)
	if err != nil {
		return err
	}
	res, execErr := s.db.ExecContext(ctx, `
		UPDATE works
		SET assignment_id = $2, session_id = $3, task_id = $4, run_task_id = $5,
		    goal = $6, status = $7, constraint_data = $8, worker_id = $9,
		    attempt = $10, stage = $11, detail = $12, result = $13, err = $14,
		    started_at = $15, finished_at = $16, created_at = $17, updated_at = $18
		WHERE id = $1`,
		w.ID, w.AssignmentID, w.SessionID, w.TaskID, w.RunTaskID,
		w.Goal, string(w.Status), constraint, w.WorkerID,
		w.Attempt, w.Stage, w.Detail, nullString(w.Result), nullString(w.Err),
		nullTime(w.StartedAt), nullTime(w.FinishedAt), w.CreatedAt, w.UpdatedAt)
	if execErr != nil {
		return convertErr(execErr, "")
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

// DeleteByRunTask 删除某 RunTask 下的全部 Work。
//
// 与内存实现一致：删除 0 行也返回 nil（幂等），因为这是级联清理而非业务删除。
func (s *WorkStore) DeleteByRunTask(ctx context.Context, runTaskID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM works WHERE run_task_id = $1`, runTaskID)
	return convertErr(err, "")
}

// scanWork 把一行扫描成 Work。
func scanWork(r rowScanner) (*domain.Work, error) {
	var (
		w          domain.Work
		status     string
		constraint []byte
		result     sql.NullString
		errText    sql.NullString
		startedAt  sql.NullTime
		finishedAt sql.NullTime
	)
	err := r.Scan(&w.ID, &w.AssignmentID, &w.SessionID, &w.TaskID, &w.RunTaskID, &w.Goal,
		&status, &constraint, &w.WorkerID, &w.Attempt, &w.Stage, &w.Detail,
		&result, &errText, &startedAt, &finishedAt, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, convertErr(err, "")
	}
	w.Status = domain.WorkStatus(status)
	w.Result = result.String
	w.Err = errText.String
	w.StartedAt = startedAt.Time
	w.FinishedAt = finishedAt.Time

	parsed, err := unmarshalConstraint(constraint)
	if err != nil {
		return nil, err
	}
	w.Constraint = parsed
	return &w, nil
}

// marshalConstraint 把 WorkConstraint 编码为 JSONB 值。
//
// nil 编码为 SQL NULL（而非 "null"），与内存实现的 nil 语义一致。
func marshalConstraint(c *domain.WorkConstraint) (any, error) {
	if c == nil {
		return nil, nil
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("storage: marshal work constraint: %w", err)
	}
	return body, nil
}

// unmarshalConstraint 把 JSONB 解回 WorkConstraint；空值返回 nil。
func unmarshalConstraint(body []byte) (*domain.WorkConstraint, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var c domain.WorkConstraint
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("storage: unmarshal work constraint: %w", err)
	}
	return &c, nil
}

// nullTime 把零值时间转成 SQL NULL。
//
// 避免写入 0001-01-01（Go 零值）——那会让前端显示异常时间，
// 也会让"未开始 / 未结束"与"很久以前"无法区分。
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
