// Package memory 提供进程内存储实现，用于阶段 0 的本地运行与测试。
//
// 该实现不跨进程持久化，仅用于验证 Session/Task 的存取语义。
// TaskStore.Create 在单把互斥锁内完成唯一性检查与写入，
// 原子地保证"一个 Session 只对应一个 Task"。
package memory

import (
	"context"
	"sync"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// SessionStore 是 SessionStore 接口的进程内实现。
type SessionStore struct {
	mu sync.RWMutex
	m  map[string]*domain.Session
}

// TaskStore 是 TaskStore 接口的进程内实现。
type TaskStore struct {
	mu sync.RWMutex
	m  map[string]*domain.Task
}

var _ storage.SessionStore = (*SessionStore)(nil)
var _ storage.TaskStore = (*TaskStore)(nil)

// NewSessionStore 创建一个空的 SessionStore。
func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*domain.Session)}
}

// NewTaskStore 创建一个空的 TaskStore。
func NewTaskStore() *TaskStore {
	return &TaskStore{m: make(map[string]*domain.Task)}
}

// Create 保存一个 Session。
func (s *SessionStore) Create(_ context.Context, sess *domain.Session) error {
	if sess == nil || sess.ID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.m[sess.ID] = &cp
	return nil
}

// Get 按 ID 返回 Session，不存在时返回 storage.ErrNotFound。
func (s *SessionStore) Get(_ context.Context, id string) (*domain.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *sess
	return &cp, nil
}

// List 返回全部 Session。
func (s *SessionStore) List(_ context.Context) ([]*domain.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Session, 0, len(s.m))
	for _, sess := range s.m {
		cp := *sess
		out = append(out, &cp)
	}
	return out, nil
}

// Delete 删除指定 Session，不存在时返回 storage.ErrNotFound。
func (s *SessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.m, id)
	return nil
}

// Create 保存一个 Task，并原子地保证"一个 Session 只对应一个 Task"。
// 当该 Session 已存在其他 Task 时返回 storage.ErrSessionHasTask。
func (s *TaskStore) Create(_ context.Context, t *domain.Task) error {
	if t == nil || t.ID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.m {
		if existing.ID == t.ID {
			continue // 同一 Task 的幂等重复创建
		}
		if existing.SessionID == t.SessionID {
			return storage.ErrSessionHasTask
		}
	}
	cp := *t
	s.m[t.ID] = &cp
	return nil
}

// Get 按 ID 返回 Task，不存在时返回 storage.ErrNotFound。
func (s *TaskStore) Get(_ context.Context, id string) (*domain.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.m[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// ListBySession 返回属于指定 Session 的全部 Task。
// 在"一个 Session 只对应一个 Task"约束下，结果最多一个元素。
func (s *TaskStore) ListBySession(_ context.Context, sessionID string) ([]*domain.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Task, 0)
	for _, t := range s.m {
		if t.SessionID == sessionID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}

// Delete 删除指定 Task，不存在时返回 storage.ErrNotFound。
func (s *TaskStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.m, id)
	return nil
}
