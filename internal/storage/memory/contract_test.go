package memory_test

import (
	"testing"

	rsmemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/storetest"
)

// TestContract 让内存实现跑一遍存储契约测试。
//
// 与 PostgreSQL 实现共用同一套断言（internal/storage/storetest），
// 保证两种存储在语义上完全一致：换数据库不该改变行为。
func TestContract(t *testing.T) {
	storetest.RunAll(t, func(t *testing.T) *storetest.Stores {
		t.Helper()
		return &storetest.Stores{
			Sessions:    memory.NewSessionStore(),
			Tasks:       memory.NewTaskStore(),
			RunTasks:    memory.NewRunTaskStore(),
			Assignments: memory.NewAssignmentStore(),
			Works:       memory.NewWorkStore(),
			States:      rsmemory.NewStore(),
		}
	}, storetest.Capabilities{})
}
