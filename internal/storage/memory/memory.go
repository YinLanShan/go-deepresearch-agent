// Package memory 提供进程内存储实现，用于阶段 0 的本地运行与测试。
//
// 该实现不跨进程持久化，仅用于验证 Session/Task/RunTask 的存取语义。
// 唯一性约束（一个 Session 一个 Task、一个 Task 一个 RunTask）都在
// 单把互斥锁内完成检查与写入，原子地保证并发下不产生重复。
package memory

import (
	"context"
	"sort"
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

// RunTaskStore 是 RunTaskStore 接口的进程内实现。
type RunTaskStore struct {
	mu     sync.RWMutex
	m      map[string]*domain.RunTask // 按 RunTask ID
	byTask map[string]string          // TaskID -> RunTaskID，保证一对一
}

var _ storage.SessionStore = (*SessionStore)(nil)
var _ storage.TaskStore = (*TaskStore)(nil)
var _ storage.RunTaskStore = (*RunTaskStore)(nil)

// NewSessionStore 创建一个空的 SessionStore。
func NewSessionStore() *SessionStore {
	return &SessionStore{m: make(map[string]*domain.Session)}
}

// NewTaskStore 创建一个空的 TaskStore。
func NewTaskStore() *TaskStore {
	return &TaskStore{m: make(map[string]*domain.Task)}
}

// NewRunTaskStore 创建一个空的 RunTaskStore。
func NewRunTaskStore() *RunTaskStore {
	return &RunTaskStore{
		m:      make(map[string]*domain.RunTask),
		byTask: make(map[string]string),
	}
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

// Create 保存一个 Task。一个 Session 可对应多个 Task，不做唯一性约束。
func (s *TaskStore) Create(_ context.Context, t *domain.Task) error {
	if t == nil || t.ID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// ListBySession 返回属于指定 Session 的全部 Task，按创建时间升序排列。
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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Update 用传入的 Task 覆盖同 ID 的既有 Task，不存在时返回 storage.ErrNotFound。
func (s *TaskStore) Update(_ context.Context, t *domain.Task) error {
	if t == nil || t.ID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[t.ID]; !ok {
		return storage.ErrNotFound
	}
	cp := *t
	s.m[t.ID] = &cp
	return nil
}

// Delete 删除指定 Task，不存在时返回 storage.ErrNotFound。
//
// 注意：本方法只删除 Task 本身，不级联删除其关联的 RunTask。
// RunTask（及其运行时）拥有独立于 Task 增删的生命周期。
func (s *TaskStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return storage.ErrNotFound
	}
	delete(s.m, id)
	return nil
}

// Create 保存一个 RunTask，并原子地保证"一个 Task 最多对应一个 RunTask"。
// 当该 Task 已存在其他 RunTask 时返回 storage.ErrRunTaskExists。
func (s *RunTaskStore) Create(_ context.Context, rt *domain.RunTask) error {
	if rt == nil || rt.ID == "" || rt.TaskID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existingID, ok := s.byTask[rt.TaskID]; ok && existingID != rt.ID {
		return storage.ErrRunTaskExists
	}
	cp := *rt
	s.m[rt.ID] = &cp
	s.byTask[rt.TaskID] = rt.ID
	return nil
}

// Get 按 RunTaskID 返回 RunTask，不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) Get(_ context.Context, id string) (*domain.RunTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt, ok := s.m[id]
	if !ok {
		return nil, storage.ErrNotFound
	}
	cp := *rt
	return &cp, nil
}

// GetByTask 返回指定 Task 对应的 RunTask，不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) GetByTask(_ context.Context, taskID string) (*domain.RunTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byTask[taskID]
	if !ok {
		return nil, storage.ErrNotFound
	}
	rt := s.m[id]
	cp := *rt
	return &cp, nil
}

// List 返回全部 RunTask。
func (s *RunTaskStore) List(_ context.Context) ([]*domain.RunTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.RunTask, 0, len(s.m))
	for _, rt := range s.m {
		cp := *rt
		out = append(out, &cp)
	}
	return out, nil
}

// Update 用传入的 RunTask 覆盖同 ID 的既有 RunTask，不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) Update(_ context.Context, rt *domain.RunTask) error {
	if rt == nil || rt.ID == "" {
		return storage.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[rt.ID]; !ok {
		return storage.ErrNotFound
	}
	cp := *rt
	s.m[rt.ID] = &cp
	return nil
}

// Delete 删除指定 RunTask，不存在时返回 storage.ErrNotFound。
func (s *RunTaskStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.m[id]
	if !ok {
		return storage.ErrNotFound
	}
	delete(s.m, id)
	delete(s.byTask, rt.TaskID)
	return nil
}
