package postgres

// 本文件是 ResearchState 的阶段 4：Results / Findings / CommitLog / Messages
// 四张子表的读写，以及依赖它们的业务操作（Commit / RevokeResult /
// AppendResult / AccumulateInterruptedContext / AppendMessage）。
//
// 与内存实现保持一致的关键语义（易错点）：
//
//  1. turn 取自 Findings 数量 + 1，而不是 Results 数量。
//  2. Commit 的 Version 只自增一次，与该批 Result 条数无关。
//  3. 空白内容被跳过；全为空白时 Commit 仍会自增 Version 并清空
//     InterruptedContext（内存实现如此）。
//  4. RevokeResult 未命中任何 Result 时**不自增 Version**，返回 0。
//  5. AppendResult 的 turn 取 len(Results)+1（逐条递增），Reason 固定为"迟到产物"。
//  6. Commit 与 AppendResult 都不受 Revoke 影响的门控限制不同：
//     Commit 受门控限制，AppendResult 不受。

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// Commit 追加一批 Result + 至多一条 Finding，并清空 InterruptedContext。
//
// 全部动作在**一个事务**内完成：Results 插入、Finding 插入、Version 自增、
// CommitLog 写入、InterruptedContext 清空。任一步失败则整体回滚，
// 不会出现"证据写进去了但版本没涨"的半提交状态。
func (s *ResearchStateStore) Commit(ctx context.Context, taskID string, in researchstate.CommitInput) error {
	if !s.gate.accepting(taskID) {
		return researchstate.ErrNotAccepting
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return convertStateErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		stateID string
		version int64
	)
	// FOR UPDATE 锁住该行，保证 version 自增与 turn 计算基于最新值。
	if err := tx.QueryRowContext(ctx, `
		SELECT id, version FROM research_states WHERE task_id = $1 FOR UPDATE`, taskID).
		Scan(&stateID, &version); err != nil {
		return convertStateErr(err)
	}

	// turn 的来源是 Findings 数量，不是 Results（与内存实现一致）。
	var findingCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM research_findings WHERE state_id = $1`, stateID).
		Scan(&findingCount); err != nil {
		return convertStateErr(err)
	}
	turn := findingCount + 1
	version++

	now := time.Now().UTC()

	records := make([]commitRecord, 0, len(in.Results)+1)

	for _, item := range in.Results {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		resultID := ids.New()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO research_results
				(id, state_id, assignment_id, turn, status, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			resultID, stateID, item.AssignmentID, turn,
			string(researchstate.ResultCommitted), content, now); err != nil {
			return convertStateErr(err)
		}
		records = append(records, commitRecord{
			Op:           researchstate.OpCommitResult,
			Turn:         turn,
			AssignmentID: item.AssignmentID,
			ResultID:     resultID,
			Content:      content,
			Version:      version,
			CreatedAt:    now,
		})
	}

	if finding := strings.TrimSpace(in.FindingContent); finding != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO research_findings (id, state_id, turn, content, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			ids.New(), stateID, turn, finding, now); err != nil {
			return convertStateErr(err)
		}
		records = append(records, commitRecord{
			Op:        researchstate.OpCommitFinding,
			Turn:      turn,
			Content:   finding,
			Version:   version,
			CreatedAt: now,
		})
	}

	// 中断原文已并入本轮 Finding，作为中间产物清空。
	if _, err := tx.ExecContext(ctx, `
		UPDATE research_states
		SET version = $2, interrupted_context = NULL, updated_at = $3
		WHERE id = $1`, stateID, version, now); err != nil {
		return convertStateErr(err)
	}

	if err := insertCommitRecords(ctx, tx, stateID, records); err != nil {
		return err
	}

	return convertStateErr(tx.Commit())
}

// RevokeResult 撤销某 Assignment 已提交的 Result。
//
// 使用 PostgreSQL 的 DELETE ... RETURNING：一条语句拿到被删内容，
// 随后把这些内容写入 CommitLog，全程在一个事务内。
//
// 语义要点：
//   - 不受提交门控限制（研究已结束也允许撤销已提交结果）；
//   - assignmentID 为空或未命中任何 Result → 返回 (0, nil)，**不自增 Version**。
func (s *ResearchStateStore) RevokeResult(ctx context.Context, taskID, assignmentID, reason string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, convertStateErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		stateID string
		version int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, version FROM research_states WHERE task_id = $1 FOR UPDATE`, taskID).
		Scan(&stateID, &version); err != nil {
		return 0, convertStateErr(err)
	}
	if assignmentID == "" {
		return 0, nil
	}

	type revokedRow struct {
		id      string
		turn    int
		content string
	}
	rows, err := tx.QueryContext(ctx, `
		DELETE FROM research_results
		WHERE state_id = $1 AND assignment_id = $2
		RETURNING id, turn, content`, stateID, assignmentID)
	if err != nil {
		return 0, convertStateErr(err)
	}
	var revoked []revokedRow
	for rows.Next() {
		var r revokedRow
		if err := rows.Scan(&r.id, &r.turn, &r.content); err != nil {
			rows.Close()
			return 0, convertStateErr(err)
		}
		revoked = append(revoked, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, convertStateErr(err)
	}
	if len(revoked) == 0 {
		return 0, nil // 没有可撤销的 Result：不改版本、不写日志
	}

	version++
	now := time.Now().UTC()

	records := make([]commitRecord, 0, len(revoked))
	for _, r := range revoked {
		records = append(records, commitRecord{
			Op:           researchstate.OpRevokeResult,
			Turn:         r.turn,
			AssignmentID: assignmentID,
			ResultID:     r.id,
			Content:      r.content,
			Reason:       reason,
			Version:      version,
			CreatedAt:    now,
		})
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE research_states SET version = $2, updated_at = $3 WHERE id = $1`,
		stateID, version, now); err != nil {
		return 0, convertStateErr(err)
	}
	if err := insertCommitRecords(ctx, tx, stateID, records); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, convertStateErr(err)
	}
	return len(revoked), nil
}

// AppendResult 仅追加 Result（用于任务中止后到达的迟到产物）。
//
// 不产生 Finding、不触碰 InterruptedContext、**不受提交门控限制**。
//
// Turn 与 Commit 使用**同一基准**（Findings 数量 + 1）：它表示"第几轮研究"，
// 而不是"第几条证据"。迟到产物不产生 Finding，因此同一次补录的多条 Result
// 共享同一个 Turn；若按 Result 条数递增会出现轮次跳号。
// CommitLog 的 Reason 固定为"迟到产物"。
func (s *ResearchStateStore) AppendResult(ctx context.Context, taskID string, in researchstate.CommitInput) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return convertStateErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		stateID string
		version int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT id, version FROM research_states WHERE task_id = $1 FOR UPDATE`, taskID).
		Scan(&stateID, &version); err != nil {
		return convertStateErr(err)
	}

	// Turn 取自 Findings 数量，与 Commit 保持同一基准。
	var findingCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM research_findings WHERE state_id = $1`, stateID).
		Scan(&findingCount); err != nil {
		return convertStateErr(err)
	}

	// 与内存实现一致：Version 无条件自增一次（即使全部内容为空）。
	version++
	now := time.Now().UTC()

	records := make([]commitRecord, 0, len(in.Results))
	turn := findingCount + 1
	for _, item := range in.Results {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		resultID := ids.New()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO research_results
				(id, state_id, assignment_id, turn, status, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			resultID, stateID, item.AssignmentID, turn,
			string(researchstate.ResultCommitted), content, now); err != nil {
			return convertStateErr(err)
		}
		records = append(records, commitRecord{
			Op:           researchstate.OpCommitResult,
			Turn:         turn,
			AssignmentID: item.AssignmentID,
			ResultID:     resultID,
			Content:      content,
			Reason:       "迟到产物",
			Version:      version,
			CreatedAt:    now,
		})
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE research_states SET version = $2, updated_at = $3 WHERE id = $1`,
		stateID, version, now); err != nil {
		return convertStateErr(err)
	}
	if err := insertCommitRecords(ctx, tx, stateID, records); err != nil {
		return err
	}
	return convertStateErr(tx.Commit())
}

// AccumulateInterruptedContext 累积追加被中断的原文。
//
// 用 SQL 的字符串拼接一次完成（interrupted_context || $2），
// 避免"读-改-写"的并发覆盖。
func (s *ResearchStateStore) AccumulateInterruptedContext(ctx context.Context, taskID, text string) error {
	if text == "" {
		return nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE research_states
		SET interrupted_context = coalesce(interrupted_context, '') || $2,
		    updated_at = now()
		WHERE task_id = $1`, taskID, text)
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

// AppendMessage 追加一条对话历史。
func (s *ResearchStateStore) AppendMessage(ctx context.Context, taskID string, role researchstate.MessageRole, content string) error {
	if content == "" {
		return nil
	}
	var stateID string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id FROM research_states WHERE task_id = $1`, taskID).Scan(&stateID); err != nil {
		return convertStateErr(err)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO research_messages (state_id, role, content, created_at)
		VALUES ($1, $2, $3, now())`, stateID, string(role), content)
	return convertStateErr(err)
}

// ─── 内部辅助 ───────────────────────────────────────────────────────────────

// commitRecord 是一条待写入的提交历史。
type commitRecord struct {
	Op           researchstate.CommitOperation
	Turn         int
	AssignmentID string
	ResultID     string
	Content      string
	Reason       string
	Version      int64
	CreatedAt    time.Time
}

// insertCommitRecords 批量写入提交历史。
func insertCommitRecords(ctx context.Context, tx *sql.Tx, stateID string, records []commitRecord) error {
	for _, r := range records {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO research_commit_log
				(state_id, op, turn, assignment_id, result_id, content, reason,
				 version, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			stateID, string(r.Op), r.Turn, r.AssignmentID, r.ResultID,
			nullString(r.Content), r.Reason, r.Version, r.CreatedAt); err != nil {
			return convertStateErr(err)
		}
	}
	return nil
}

// loadCollections 载入四张子表，补齐 State 的集合字段。
//
// 阶段 4 之后 Get 返回完整状态（此前只返回标量）。
func (s *ResearchStateStore) loadCollections(ctx context.Context, st *researchstate.State) error {
	if st == nil || st.ID == "" {
		return nil
	}

	// Results
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, assignment_id, turn, status, content, created_at
		FROM research_results WHERE state_id = $1
		ORDER BY seq ASC`, st.ID)
	if err != nil {
		return convertStateErr(err)
	}
	results := make([]researchstate.Result, 0)
	for rows.Next() {
		var (
			r      researchstate.Result
			status string
		)
		if err := rows.Scan(&r.ID, &r.AssignmentID, &r.Turn, &status,
			&r.Content, &r.CreatedAt); err != nil {
			rows.Close()
			return convertStateErr(err)
		}
		r.Status = researchstate.ResultStatus(status)
		results = append(results, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return convertStateErr(err)
	}
	st.Results = results

	// Findings
	rows, err = s.db.QueryContext(ctx, `
		SELECT turn, content, created_at FROM research_findings
		WHERE state_id = $1
		ORDER BY seq ASC`, st.ID)
	if err != nil {
		return convertStateErr(err)
	}
	findings := make([]researchstate.Finding, 0)
	for rows.Next() {
		var f researchstate.Finding
		if err := rows.Scan(&f.Turn, &f.Content, &f.CreatedAt); err != nil {
			rows.Close()
			return convertStateErr(err)
		}
		findings = append(findings, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return convertStateErr(err)
	}
	st.Findings = findings

	// CommitLog
	rows, err = s.db.QueryContext(ctx, `
		SELECT op, turn, assignment_id, result_id, coalesce(content, ''),
		       reason, version, created_at
		FROM research_commit_log WHERE state_id = $1
		ORDER BY seq ASC`, st.ID)
	if err != nil {
		return convertStateErr(err)
	}
	logs := make([]researchstate.CommitRecord, 0)
	for rows.Next() {
		var (
			rec researchstate.CommitRecord
			op  string
		)
		if err := rows.Scan(&op, &rec.Turn, &rec.AssignmentID, &rec.ResultID,
			&rec.Content, &rec.Reason, &rec.Version, &rec.CreatedAt); err != nil {
			rows.Close()
			return convertStateErr(err)
		}
		rec.Op = researchstate.CommitOperation(op)
		logs = append(logs, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return convertStateErr(err)
	}
	// Seq 在库中是全局自增，这里按 state 内部顺序重新编号（从 1 开始），
	// 与内存实现的 AppendCommitRecord 语义一致。
	for i := range logs {
		logs[i].Seq = int64(i + 1)
	}
	st.CommitLog = logs

	// History
	rows, err = s.db.QueryContext(ctx, `
		SELECT role, content, created_at FROM research_messages
		WHERE state_id = $1
		ORDER BY seq ASC`, st.ID)
	if err != nil {
		return convertStateErr(err)
	}
	history := make([]researchstate.Message, 0)
	for rows.Next() {
		var (
			m    researchstate.Message
			role string
		)
		if err := rows.Scan(&role, &m.Content, &m.CreatedAt); err != nil {
			rows.Close()
			return convertStateErr(err)
		}
		m.Role = researchstate.MessageRole(role)
		history = append(history, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return convertStateErr(err)
	}
	st.History = history

	return nil
}
