// Package checkpoint 提供 ADK Checkpoint 的进程内存储实现。
//
// Eino 的 adk.CheckPointStore 只要求 Get/Set 两个方法；本项目补一个 Delete
// 以满足 adk.CheckPointDeleter，便于中止/删除任务时清理残留 checkpoint。
//
// 说明：这里只负责"字节存取"，序列化与恢复语义完全由 Eino ADK 负责，
// 项目不自行实现中断/恢复逻辑。
package checkpoint

import (
	"context"
	"sync"
)

// MemoryStore 是 adk.CheckPointStore 的进程内实现。
type MemoryStore struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemoryStore 创建一个空的进程内 Checkpoint 存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: make(map[string][]byte)}
}

// Get 返回指定 ID 的 checkpoint 字节与是否存在。
func (s *MemoryStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.m[checkPointID]
	if !ok {
		return nil, false, nil
	}
	// 返回副本，避免调用方改动内部数据。
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, true, nil
}

// Set 写入 checkpoint。
func (s *MemoryStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(checkPoint))
	copy(cp, checkPoint)
	s.m[checkPointID] = cp
	return nil
}

// Delete 删除 checkpoint（实现 adk.CheckPointDeleter）。
func (s *MemoryStore) Delete(_ context.Context, checkPointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, checkPointID)
	return nil
}
