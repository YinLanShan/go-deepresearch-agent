// Package memory 提供 ResearchState 的进程内存储实现。
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
)

// Store 是 researchstate.Store 的进程内实现，按 SessionID 组织。
type Store struct {
	mu        sync.RWMutex
	m         map[string]*researchstate.State
	accepting map[string]bool // sessionID -> 是否接受新的 Commit
}

var _ researchstate.Store = (*Store)(nil)

// NewStore 创建一个空的 Store。
func NewStore() *Store {
	return &Store{
		m:         make(map[string]*researchstate.State),
		accepting: make(map[string]bool),
	}
}

// Create 保存一份研究状态，并开启提交门控。
func (s *Store) Create(_ context.Context, st *researchstate.State) error {
	if st == nil || st.SessionID == "" {
		return researchstate.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[st.SessionID] = clone(st)
	s.accepting[st.SessionID] = true
	return nil
}

// Get 按 SessionID 返回研究状态，不存在时返回 researchstate.ErrNotFound。
func (s *Store) Get(_ context.Context, sessionID string) (*researchstate.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.m[sessionID]
	if !ok {
		return nil, researchstate.ErrNotFound
	}
	return clone(st), nil
}

// Commit 追加一轮研究产物（Result）与研究结论（Finding），
// 清空已合并进 Finding 的被中断上下文，并自增版本号。
//
// 若提交门控已关闭（中止状态），返回 researchstate.ErrNotAccepting；
// 正在进行的 Commit（已获得锁）会继续完成。
func (s *Store) Commit(_ context.Context, sessionID string, in researchstate.CommitInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[sessionID]
	if !ok {
		return researchstate.ErrNotFound
	}
	if !s.accepting[sessionID] {
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
	// 被中断的上下文已合并进本轮 Finding，清空。
	st.InterruptedContext = ""
	st.Version++
	st.UpdatedAt = now
	return nil
}

// AccumulateInterruptedContext 累积追加被中断的上下文原文（多次中断累积到同一字段）。
func (s *Store) AccumulateInterruptedContext(_ context.Context, sessionID, text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[sessionID]
	if !ok {
		return researchstate.ErrNotFound
	}
	st.InterruptedContext += text
	st.UpdatedAt = time.Now()
	return nil
}

// StopCommits 关闭提交门控：此后新的 Commit 返回 ErrNotAccepting。
func (s *Store) StopCommits(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[sessionID]; !ok {
		return researchstate.ErrNotFound
	}
	s.accepting[sessionID] = false
	return nil
}

// ResumeCommits 重新开启提交门控（中止后恢复研究时调用）。
func (s *Store) ResumeCommits(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[sessionID]; !ok {
		return researchstate.ErrNotFound
	}
	s.accepting[sessionID] = true
	return nil
}

// Delete 删除指定 Session 的研究状态，不存在时返回 researchstate.ErrNotFound。
func (s *Store) Delete(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[sessionID]; !ok {
		return researchstate.ErrNotFound
	}
	delete(s.m, sessionID)
	delete(s.accepting, sessionID)
	return nil
}

// clone 深拷贝一份 State（含切片），避免调用方改动内部数据。
func clone(st *researchstate.State) *researchstate.State {
	cp := *st
	cp.Results = append([]researchstate.Result(nil), st.Results...)
	cp.Findings = append([]researchstate.Finding(nil), st.Findings...)
	cp.RelatedFindings = append([]researchstate.RelatedFinding(nil), st.RelatedFindings...)
	return &cp
}
