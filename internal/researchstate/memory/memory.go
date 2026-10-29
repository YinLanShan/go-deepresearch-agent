// Package memory 提供 ResearchState 的进程内存储实现。
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// Store 是 researchstate.Store 的进程内实现，按 TaskID 组织。
type Store struct {
	mu        sync.RWMutex
	m         map[string]*researchstate.State // taskID -> State
	accepting map[string]bool                 // taskID -> 是否接受新的 Commit
	latest    map[string]string               // sessionID -> 最近创建的 taskID
}

var _ researchstate.Store = (*Store)(nil)

// NewStore 创建一个空的 Store。
func NewStore() *Store {
	return &Store{
		m:         make(map[string]*researchstate.State),
		accepting: make(map[string]bool),
		latest:    make(map[string]string),
	}
}

// Create 保存一份研究状态（按 TaskID），并开启提交门控。
//
// 错误语义：参数为空 → ErrInvalidArgument；同 Task 已存在 → ErrAlreadyExists
// （一个 Task 只能有一份研究状态，重复创建是调用方逻辑错误，不得静默覆盖）。
func (s *Store) Create(_ context.Context, st *researchstate.State) error {
	if st == nil || st.TaskID == "" {
		return researchstate.ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[st.TaskID]; ok {
		return researchstate.ErrAlreadyExists
	}
	s.m[st.TaskID] = clone(st)
	s.accepting[st.TaskID] = true
	// 记录该 Session 最近创建的研究状态，供 GetLatestBySession 使用。
	s.latest[st.SessionID] = st.TaskID
	return nil
}

// Get 按 TaskID 返回研究状态，不存在时返回 researchstate.ErrNotFound。
func (s *Store) Get(_ context.Context, taskID string) (*researchstate.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[taskID]
	if !ok {
		return nil, researchstate.ErrNotFound
	}
	return clone(st), nil
}

// GetLatestBySession 返回指定 Session 下最新序号的 ResearchState。
func (s *Store) GetLatestBySession(_ context.Context, sessionID string) (*researchstate.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	taskID, ok := s.latest[sessionID]
	if !ok {
		return nil, researchstate.ErrNotFound
	}
	st, ok := s.m[taskID]
	if !ok {
		return nil, researchstate.ErrNotFound
	}
	return clone(st), nil
}

// ListBySession 返回指定 Session 下全部 ResearchState，按序号升序。
func (s *Store) ListBySession(_ context.Context, sessionID string) ([]*researchstate.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*researchstate.State, 0)
	for _, st := range s.m {
		if st.SessionID == sessionID {
			out = append(out, clone(st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

// Commit 追加一批研究产物（Result）与至多一条过程输出（Finding），并自增版本号。
//
// InterruptedContext 是中间产物：其内容已由调用方并入本轮 Finding，
// 因此这里在提交成功后清空该字段。每次提交都会写入 CommitLog。
// 若提交门控已关闭（被挤掉的等待状态），返回 researchstate.ErrNotAccepting。
func (s *Store) Commit(_ context.Context, taskID string, in researchstate.CommitInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	if !s.accepting[taskID] {
		return researchstate.ErrNotAccepting
	}
	now := time.Now()
	turn := len(st.Findings) + 1
	st.Version++

	// 逐条保存 Result，并记录提交历史（撤销时按 AssignmentID 定位）。
	for _, item := range in.Results {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		result := researchstate.Result{
			ID:           ids.New(),
			AssignmentID: item.AssignmentID,
			Turn:         turn,
			Content:      content,
			Status:       researchstate.ResultCommitted,
			CreatedAt:    now,
		}
		st.Results = append(st.Results, result)
		st.AppendCommitRecord(researchstate.CommitRecord{
			Op:           researchstate.OpCommitResult,
			Turn:         turn,
			AssignmentID: item.AssignmentID,
			ResultID:     result.ID,
			Content:      content,
			Version:      st.Version,
			CreatedAt:    now,
		})
	}

	if finding := strings.TrimSpace(in.FindingContent); finding != "" {
		st.Findings = append(st.Findings, researchstate.Finding{
			Turn:      turn,
			Content:   finding,
			CreatedAt: now,
		})
		st.AppendCommitRecord(researchstate.CommitRecord{
			Op:        researchstate.OpCommitFinding,
			Turn:      turn,
			Content:   finding,
			Version:   st.Version,
			CreatedAt: now,
		})
	}

	// 中断原文已并入本轮 Finding，作为中间产物清空。
	st.InterruptedContext = ""
	st.UpdatedAt = now
	return nil
}

// RevokeResult 撤销某个 Assignment 已提交的 Result。
//
// 撤销也是一个提交动作：从 Results 中物理移除对应条目（使其不再出现在
// ResearchState 与上下文中），并把被撤销的内容写入 CommitLog 以便审计。
// 不受提交门控限制——即使研究已结束，也允许撤销此前合法提交的结果。
// 返回被撤销的 Result 数量。
func (s *Store) RevokeResult(_ context.Context, taskID, assignmentID, reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return 0, researchstate.ErrNotFound
	}
	if assignmentID == "" {
		return 0, nil
	}

	now := time.Now()
	kept := make([]researchstate.Result, 0, len(st.Results))
	var revoked []researchstate.Result
	for _, r := range st.Results {
		if r.AssignmentID == assignmentID {
			revoked = append(revoked, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(revoked) == 0 {
		return 0, nil
	}
	st.Results = kept
	st.Version++
	for _, r := range revoked {
		st.AppendCommitRecord(researchstate.CommitRecord{
			Op:           researchstate.OpRevokeResult,
			Turn:         r.Turn,
			AssignmentID: assignmentID,
			ResultID:     r.ID,
			Content:      r.Content,
			Reason:       reason,
			Version:      st.Version,
			CreatedAt:    now,
		})
	}
	st.UpdatedAt = now
	return len(revoked), nil
}

// SetConclusion 写入该研究节点的最终结论。
//
// 仅由 finish_research 触发；未结束的研究该字段保持为空。
func (s *Store) SetConclusion(_ context.Context, taskID, conclusion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	now := time.Now()
	st.Conclusion = conclusion
	st.Version++
	st.AppendCommitRecord(researchstate.CommitRecord{
		Op:        researchstate.OpSetConclusion,
		Content:   conclusion,
		Version:   st.Version,
		CreatedAt: now,
	})
	st.UpdatedAt = now
	return nil
}

// AddRequirement 追加一条用户需求（按时间顺序累积）。
func (s *Store) AddRequirement(_ context.Context, taskID, requirement string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	if st.AddRequirement(requirement) {
		st.UpdatedAt = time.Now()
	}
	return nil
}

// AppendResult 仅追加一条 Result（用于任务中止后到达的迟到产物）。
//
// 不产生 Finding、不触碰 InterruptedContext、不受提交门控限制。
//
// Turn 与 Commit 使用**同一基准**（Findings 数量 + 1）：它表示"第几轮研究"，
// 而不是"第几条证据"。迟到产物本身不产生 Finding，因此同一次补录的多条
// Result 共享同一个 Turn；若按 Result 条数递增会出现轮次跳号。
func (s *Store) AppendResult(_ context.Context, taskID string, in researchstate.CommitInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	now := time.Now()
	turn := len(st.Findings) + 1
	st.Version++
	for _, item := range in.Results {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		result := researchstate.Result{
			ID:           ids.New(),
			AssignmentID: item.AssignmentID,
			Turn:         turn,
			Content:      content,
			Status:       researchstate.ResultCommitted,
			CreatedAt:    now,
		}
		st.Results = append(st.Results, result)
		st.AppendCommitRecord(researchstate.CommitRecord{
			Op:           researchstate.OpCommitResult,
			Turn:         result.Turn,
			AssignmentID: item.AssignmentID,
			ResultID:     result.ID,
			Content:      content,
			Reason:       "迟到产物",
			Version:      st.Version,
			CreatedAt:    now,
		})
	}
	st.UpdatedAt = now
	return nil
}

// AccumulateInterruptedContext 累积追加被中断的上下文原文（多次中断累积到同一字段）。
func (s *Store) AccumulateInterruptedContext(_ context.Context, taskID, text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	st.InterruptedContext += text
	st.UpdatedAt = time.Now()
	return nil
}

// AppendMessage 追加一条对话历史。
func (s *Store) AppendMessage(_ context.Context, taskID string, role researchstate.MessageRole, content string) error {
	if content == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	st.AddMessage(role, content)
	return nil
}

// StopCommits 关闭提交门控：此后新的 Commit 返回 ErrNotAccepting。
//
// 门控是派生状态：条目不存在时也直接记录（幂等、自愈），
// 真正创建该 Task 时 Create 会显式把门控开启为 true。
func (s *Store) StopCommits(_ context.Context, taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting[taskID] = false
}

// ResumeCommits 重新开启提交门控（恢复研究时调用），同样幂等自愈。
func (s *Store) ResumeCommits(_ context.Context, taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting[taskID] = true
}

// DeleteBySession 删除指定 Session 的全部研究状态。
func (s *Store) DeleteBySession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var idsToDelete []string
	for taskID, st := range s.m {
		if st.SessionID == sessionID {
			idsToDelete = append(idsToDelete, taskID)
		}
	}
	if len(idsToDelete) == 0 {
		return researchstate.ErrNotFound
	}
	sort.Strings(idsToDelete)
	for _, taskID := range idsToDelete {
		delete(s.m, taskID)
		delete(s.accepting, taskID)
	}
	delete(s.latest, sessionID)
	return nil
}

// clone 深拷贝一份 State（含切片），避免调用方改动内部数据。
func clone(st *researchstate.State) *researchstate.State {
	cp := *st
	cp.Results = append([]researchstate.Result(nil), st.Results...)
	cp.Findings = append([]researchstate.Finding(nil), st.Findings...)
	cp.History = append([]researchstate.Message(nil), st.History...)
	if len(st.RelatedNodes) > 0 {
		nodes := make([]researchstate.RelatedNode, len(st.RelatedNodes))
		for i, n := range st.RelatedNodes {
			n.Results = append([]researchstate.Result(nil), n.Results...)
			n.Findings = append([]researchstate.Finding(nil), n.Findings...)
			nodes[i] = n
		}
		cp.RelatedNodes = nodes
	}
	return &cp
}
