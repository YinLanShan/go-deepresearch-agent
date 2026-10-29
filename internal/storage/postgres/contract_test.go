package postgres_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/postgres"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/storetest"
)

// TestContract 让 PostgreSQL 实现跑一遍与内存实现相同的契约测试。
//
// 与 internal/storage/memory/contract_test.go 共用 storetest 断言集，
// 保证"换数据库不改变行为"。
//
// 通过环境变量开启（默认跳过，避免无 PG 环境时失败）：
//
//	DRA_TEST_PG_DSN="postgres://postgres:pass@127.0.0.1:5432/dra_test?sslmode=disable" \
//	  go test ./internal/storage/postgres/ -run TestContract -v
func TestContract(t *testing.T) {
	dsn := os.Getenv("DRA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DRA_TEST_PG_DSN not set; skipping PostgreSQL contract test")
	}

	db, err := postgres.Open(t.Context(), postgres.Config{DSN: dsn, MaxOpen: 5, MaxIdle: 2})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := postgres.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	storetest.RunAll(t, func(t *testing.T) *storetest.Stores {
		t.Helper()
		truncateAll(t, db)
		return &storetest.Stores{
			Sessions:    postgres.NewSessionStore(db),
			Tasks:       postgres.NewTaskStore(db),
			RunTasks:    postgres.NewRunTaskStore(db),
			Assignments: postgres.NewAssignmentStore(db),
			Works:       postgres.NewWorkStore(db),
			States:      postgres.NewResearchStateStore(db),
		}
	}, storetest.Capabilities{})
}

// truncateAll 清空全部业务表，保证每个契约子测试互不干扰。
func truncateAll(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		TRUNCATE sessions, tasks, run_tasks, research_states,
		         research_results, research_findings, research_commit_log,
		         research_messages, assignments, works
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}
