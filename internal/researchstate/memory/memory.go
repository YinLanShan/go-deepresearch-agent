// Package memory 提供 ResearchState 的进程内存储实现。
package memory

import (
	"context"
	"sort"
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
func (s *Store) Create(_ context.Context, st *researchstate.State) error {
	if st == nil || st.TaskID == "" {
		return researchstate.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Commit 追加一轮研究产物（Result）与研究结论（Finding），并自增版本号。
//
// InterruptedContext 不参与 Commit：它独立累积，始终作为分区进入上下文。
// 若提交门控已关闭（暂停状态），返回 researchstate.ErrNotAccepting。
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

	st.Results = append(st.Results, researchstate.Result{
		ID:        ids.New(),
		Turn:      turn,
		Content:   in.ResultContent,
		Status:    researchstate.ResultCommitted,
		CreatedAt: now,
	})
	st.Findings = append(st.Findings, researchstate.Finding{
		Turn:      turn,
		Content:   in.FindingContent,
		CreatedAt: now,
	})
	st.Version++
	st.UpdatedAt = now
	return nil
}

// AppendResult 仅追加一条 Result（用于任务中止后到达的迟到产物）。
//
// 不产生 Finding、不触碰 InterruptedContext、不受提交门控限制。
func (s *Store) AppendResult(_ context.Context, taskID string, in researchstate.CommitInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	now := time.Now()
	st.Results = append(st.Results, researchstate.Result{
		ID:        ids.New(),
		Turn:      len(st.Results) + 1,
		Content:   in.ResultContent,
		Status:    researchstate.ResultCommitted,
		CreatedAt: now,
	})
	st.Version++
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

// SetRequirementIfEmpty 在需求为空时写入（首条用户消息即研究需求）。
func (s *Store) SetRequirementIfEmpty(_ context.Context, taskID, requirement string) error {
	if requirement == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[taskID]
	if !ok {
		return researchstate.ErrNotFound
	}
	if st.SetRequirementIfEmpty(requirement) {
		st.UpdatedAt = time.Now()
	}
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
func (s *Store) StopCommits(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[taskID]; !ok {
		return researchstate.ErrNotFound
	}
	s.accepting[taskID] = false
	return nil
}

// ResumeCommits 重新开启提交门控（暂停后恢复研究时调用）。
func (s *Store) ResumeCommits(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[taskID]; !ok {
		return researchstate.ErrNotFound
	}
	s.accepting[taskID] = true
	return nil
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
