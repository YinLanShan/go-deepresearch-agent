package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// ─── ResearchStateStore（阶段 3：标量部分）─────────────────────────────────
//
// 覆盖范围（docs/DataModel.md 阶段 3）：
//   - Create / Get / GetLatestBySession / ListBySession
//   - AddRequirement / SetConclusion / DeleteBySession
//   - StopCommits / ResumeCommits（内存态，不落库）
//
// 尚未实现（阶段 4/5）：Commit / RevokeResult / AppendResult /
// AccumulateInterruptedContext / AppendMessage。
// 这些方法**显式返回 ErrNotImplemented**，绝不静默返回成功。
//
// Get 返回的 State 只包含标量与 requirements / related_nodes；
// Results / Findings / CommitLog / History 四个集合留空（阶段 4 补）。
// 因此本实现在阶段 3 **不得接入 Runtime**，否则会丢数据。
type ResearchStateStore struct {
	db *sql.DB
	// gate 保存提交门控（纯运行时状态，不落库）。
	gate *commitGate
}

// NewResearchStateStore 构造 ResearchState 存储。
func NewResearchStateStore(db *sql.DB) *ResearchStateStore {
	return &ResearchStateStore{db: db, gate: newCommitGate()}
}

// 编译期断言：必须实现 researchstate.Store。
var _ researchstate.Store = (*ResearchStateStore)(nil)

const stateColumns = `id, task_id, session_id, ordinal, conclusion,
	interrupted_context, requirements, related_nodes,
	version, created_at, updated_at`

// Create 保存一份研究状态。
//
// 错误语义：参数为空 → ErrInvalidArgument；同 Task 已存在 → ErrAlreadyExists。
// 创建时显式开启提交门控（与内存实现一致）。
func (s *ResearchStateStore) Create(ctx context.Context, st *researchstate.State) error {
	if st == nil || st.TaskID == "" {
		return researchstate.ErrInvalidArgument
	}
	requirements := st.Requirements
	if requirements == nil {
		requirements = []string{}
	}
	related, err := marshalRelatedNodes(st.RelatedNodes)
	if err != nil {
		return err
	}
	if st.ID == "" {
		return researchstate.ErrInvalidArgument
	}

	_, execErr := s.db.ExecContext(ctx, `
		INSERT INTO research_states (`+stateColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		st.ID, st.TaskID, st.SessionID, st.Ordinal, nullString(st.Conclusion),
		nullString(st.InterruptedContext), requirements, related,
		st.Version, st.CreatedAt, st.UpdatedAt)
	if execErr != nil {
		return convertStateErr(execErr)
	}
	// 创建即开启门控（覆盖任何陈旧的关闭标记）。
	s.gate.open(st.TaskID)
	return nil
}

// Get 按 TaskID 返回研究状态。
//
// 阶段 3 只返回标量与 requirements / related_nodes；
// Results / Findings / CommitLog / History 留空。
func (s *ResearchStateStore) Get(ctx context.Context, taskID string) (*researchstate.State, error) {
	return s.queryOne(ctx, `WHERE task_id = $1`, taskID)
}

// GetLatestBySession 返回该 Session 下序号最大（最新）的研究状态。
func (s *ResearchStateStore) GetLatestBySession(ctx context.Context, sessionID string) (*researchstate.State, error) {
	return s.queryOne(ctx, `WHERE session_id = $1 ORDER BY ordinal DESC LIMIT 1`, sessionID)
}

// ListBySession 返回该 Session 下全部研究状态，按序号升序。
func (s *ResearchStateStore) ListBySession(ctx context.Context, sessionID string) ([]*researchstate.State, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+stateColumns+` FROM research_states
		WHERE session_id = $1
		ORDER BY ordinal ASC`, sessionID)
	if err != nil {
		return nil, convertStateErr(err)
	}
	defer rows.Close()

	out := make([]*researchstate.State, 0)
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, convertStateErr(rows.Err())
}

// AddRequirement 追加一条用户需求（去重与空串判断交给 State.AddRequirement）。
//
// 使用单条 UPDATE 原子完成"读-改-写"，避免并发覆盖。
// 返回是否真的新增了需求：未新增时不改 UpdatedAt（与内存实现一致）。
func (s *ResearchStateStore) AddRequirement(ctx context.Context, taskID, requirement string) error {
	st, err := s.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if !st.AddRequirement(requirement) {
		return nil // 已存在或为空：不写入，也不改动时间戳
	}
	return s.updateScalars(ctx, taskID, st)
}

// SetConclusion 写入最终结论，并自增版本号、追加一条提交记录。
//
// 与内存实现一致：写 Conclusion + Version++ + CommitLog。
// 阶段 3 的 commit_log 子表已建好，这里直接写入（阶段 4 的 Commit 也用它）。
func (s *ResearchStateStore) SetConclusion(ctx context.Context, taskID, conclusion string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return convertStateErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		stateID string
		version int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, version FROM research_states WHERE task_id = $1 FOR UPDATE`, taskID).
		Scan(&stateID, &version)
	if err != nil {
		return convertStateErr(err)
	}
	version++

	if _, err := tx.ExecContext(ctx, `
		UPDATE research_states
		SET conclusion = $2, version = $3, updated_at = now()
		WHERE id = $1`,
		stateID, conclusion, version); err != nil {
		return convertStateErr(err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO research_commit_log
			(state_id, op, turn, assignment_id, result_id, content, reason, version, created_at)
		VALUES ($1, $2, 0, '', '', $3, '', $4, now())`,
		stateID, string(researchstate.OpSetConclusion), conclusion, version); err != nil {
		return convertStateErr(err)
	}

	return convertStateErr(tx.Commit())
}

// DeleteBySession 删除某 Session 下的全部研究状态。
//
// 与内存实现一致：没有任何可删对象时返回 ErrNotFound。
func (s *ResearchStateStore) DeleteBySession(ctx context.Context, sessionID string) error {
	// 先取要删的 taskID，用于同步清理门控（否则会残留关闭标记）。
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id FROM research_states WHERE session_id = $1`, sessionID)
	if err != nil {
		return convertStateErr(err)
	}
	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return convertStateErr(err)
		}
		taskIDs = append(taskIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return convertStateErr(err)
	}
	if len(taskIDs) == 0 {
		return researchstate.ErrNotFound
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM research_states WHERE session_id = $1`, sessionID); err != nil {
		return convertStateErr(err)
	}
	for _, id := range taskIDs {
		s.gate.forget(id)
	}
	return nil
}

// StopCommits 关闭提交门控（纯内存，不落库）。
//
// 与内存实现一致：永远"设置成功"，即使该 Task 尚无状态。
func (s *ResearchStateStore) StopCommits(_ context.Context, taskID string) {
	s.gate.close(taskID)
}

// ResumeCommits 重新开启提交门控。
func (s *ResearchStateStore) ResumeCommits(_ context.Context, taskID string) {
	s.gate.open(taskID)
}

// ─── 尚未实现（阶段 4/5）──────────────────────────────────────────────────
//
// 显式返回 ErrNotImplemented：绝不能返回 nil 让调用方误以为已落库。

// Commit 追加一批 Result + Finding（阶段 4 实现）。
func (s *ResearchStateStore) Commit(ctx context.Context, taskID string, in researchstate.CommitInput) error {
	if !s.gate.accepting(taskID) {
		return researchstate.ErrNotAccepting
	}
	return fmt.Errorf("%w: Commit (stage 4)", researchstate.ErrNotImplemented)
}

// RevokeResult 撤销已提交的 Result（阶段 4 实现）。
func (s *ResearchStateStore) RevokeResult(ctx context.Context, taskID, assignmentID, reason string) (int, error) {
	return 0, fmt.Errorf("%w: RevokeResult (stage 4)", researchstate.ErrNotImplemented)
}

// AppendResult 仅追加一条 Result（阶段 4 实现）。
func (s *ResearchStateStore) AppendResult(ctx context.Context, taskID string, in researchstate.CommitInput) error {
	return fmt.Errorf("%w: AppendResult (stage 4)", researchstate.ErrNotImplemented)
}

// AccumulateInterruptedContext 累积被打断的原文（阶段 4 实现，需 TEXT 拼接）。
func (s *ResearchStateStore) AccumulateInterruptedContext(ctx context.Context, taskID, text string) error {
	return fmt.Errorf("%w: AccumulateInterruptedContext (stage 4)",
		researchstate.ErrNotImplemented)
}

// AppendMessage 追加一条对话历史（阶段 4 实现）。
func (s *ResearchStateStore) AppendMessage(ctx context.Context, taskID string, role researchstate.MessageRole, content string) error {
	return fmt.Errorf("%w: AppendMessage (stage 4)", researchstate.ErrNotImplemented)
}

// ─── 内部辅助 ───────────────────────────────────────────────────────────────

// queryOne 按附加条件查询单条研究状态。
func (s *ResearchStateStore) queryOne(ctx context.Context, where string, arg any) (*researchstate.State, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+stateColumns+` FROM research_states `+where, arg)
	st, err := scanState(row)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// updateScalars 保存标量字段与 requirements（阶段 3 的写路径）。
func (s *ResearchStateStore) updateScalars(ctx context.Context, taskID string, st *researchstate.State) error {
	requirements := st.Requirements
	if requirements == nil {
		requirements = []string{}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE research_states
		SET requirements = $2, conclusion = $3, interrupted_context = $4,
		    updated_at = $5
		WHERE task_id = $1`,
		taskID, requirements, nullString(st.Conclusion),
		nullString(st.InterruptedContext), st.UpdatedAt)
	if err != nil {
		return convertStateErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return researchstate.ErrNotFound
	}
	return nil
}

// scanState 把一行扫描成 State（只填标量与两个集合列）。
func scanState(r rowScanner) (*researchstate.State, error) {
	var (
		st          researchstate.State
		conclusion  sql.NullString
		interrupted sql.NullString
		related     []byte
	)
	err := r.Scan(&st.ID, &st.TaskID, &st.SessionID, &st.Ordinal, &conclusion,
		&interrupted, &st.Requirements, &related, &st.Version,
		&st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, convertStateErr(err)
	}
	st.Conclusion = conclusion.String
	st.InterruptedContext = interrupted.String

	nodes, err := unmarshalRelatedNodes(related)
	if err != nil {
		return nil, err
	}
	st.RelatedNodes = nodes
	return &st, nil
}

// marshalRelatedNodes 把前序节点快照编码为 JSONB。
func marshalRelatedNodes(nodes []researchstate.RelatedNode) (any, error) {
	if nodes == nil {
		return []byte("[]"), nil
	}
	body, err := json.Marshal(nodes)
	if err != nil {
		return nil, fmt.Errorf("storage: marshal related nodes: %w", err)
	}
	return body, nil
}

// unmarshalRelatedNodes 把 JSONB 解回前序节点快照。
func unmarshalRelatedNodes(body []byte) ([]researchstate.RelatedNode, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var nodes []researchstate.RelatedNode
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("storage: unmarshal related nodes: %w", err)
	}
	return nodes, nil
}

// convertStateErr 把驱动错误转换为 researchstate 的哨兵错误。
func convertStateErr(err error) error {
	if err == nil {
		return nil
	}
	if err == sql.ErrNoRows {
		return researchstate.ErrNotFound
	}
	if isUniqueViolation(err) {
		return researchstate.ErrAlreadyExists
	}
	return err
}
