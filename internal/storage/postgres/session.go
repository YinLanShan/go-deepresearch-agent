package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// pgUniqueViolation 是 PostgreSQL 的唯一约束冲突错误码（23505）。
const pgUniqueViolation = "23505"

// convertErr 把驱动错误转换为 storage 的哨兵错误。
//
// 这是持久化层的关键职责：上层只认 storage.ErrNotFound / ErrAlreadyExists，
// 不应该知道 SQLSTATE 的存在。
func convertErr(err error, alreadyExistsMsg string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotFound
	}
	if isUniqueViolation(err) {
		if alreadyExistsMsg != "" {
			return fmt.Errorf("%w: %s", storage.ErrAlreadyExists, alreadyExistsMsg)
		}
		return storage.ErrAlreadyExists
	}
	return err
}

// isUniqueViolation 判断错误是否为 PostgreSQL 唯一约束冲突（23505）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	return false
}

// ─── SessionStore ───────────────────────────────────────────────────────────

// SessionStore 是 Session 的 PostgreSQL 实现。
type SessionStore struct{ db *sql.DB }

// NewSessionStore 构造 Session 存储。
func NewSessionStore(db *sql.DB) *SessionStore { return &SessionStore{db: db} }

var _ storage.SessionStore = (*SessionStore)(nil)

// Create 保存一个 Session；ID 重复时返回 storage.ErrAlreadyExists。
func (s *SessionStore) Create(ctx context.Context, sess *domain.Session) error {
	if sess == nil || sess.ID == "" {
		return storage.ErrInvalidArgument
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, title, created_at, updated_at)
		VALUES ($1, $2, $3, $4)`,
		sess.ID, sess.Title, sess.CreatedAt, sess.UpdatedAt)
	return convertErr(err, fmt.Sprintf("session %q", sess.ID))
}

// Get 按 ID 返回 Session；不存在时返回 storage.ErrNotFound。
func (s *SessionStore) Get(ctx context.Context, id string) (*domain.Session, error) {
	var out domain.Session
	err := s.db.QueryRowContext(ctx, `
		SELECT id, title, created_at, updated_at FROM sessions WHERE id = $1`, id).
		Scan(&out.ID, &out.Title, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, convertErr(err, "")
	}
	return &out, nil
}

// List 返回全部 Session，按创建时间升序。
func (s *SessionStore) List(ctx context.Context) ([]*domain.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, created_at, updated_at FROM sessions
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, convertErr(err, "")
	}
	defer rows.Close()

	out := make([]*domain.Session, 0)
	for rows.Next() {
		var s domain.Session
		if err := rows.Scan(&s.ID, &s.Title, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, convertErr(err, "")
		}
		out = append(out, &s)
	}
	return out, convertErr(rows.Err(), "")
}

// Delete 删除 Session；不存在时返回 storage.ErrNotFound。
//
// Task / RunTask / ResearchState / Assignment / Work 由外键级联删除。
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
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
