// Package postgres 提供 PostgreSQL 持久化实现。
//
// 设计契约见 docs/DataModel.md，建表脚本见 migrations/0001_init.sql。
//
// 与内存实现的关系：
//   - 两者实现同一批 storage / researchstate 接口；
//   - 内存实现保留作为参考实现与测试替身；
//   - 语义（错误哨兵、排序、拷贝语义）必须保持一致。
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	// pgx 的 database/sql 驱动（标准接口 + pgx 内核）。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsFS 内嵌建表脚本，避免运行时依赖文件路径。
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Config 是 PostgreSQL 连接配置。
type Config struct {
	// DSN 形如 postgres://user:pass@host:5432/db?sslmode=disable
	DSN string
	// MaxOpen 是最大打开连接数；<=0 时使用 20。
	MaxOpen int
	// MaxIdle 是最大空闲连接数；<=0 时使用 10。
	MaxIdle int
	// ConnMaxLifetime 是连接最长存活时间；<=0 时使用 30 分钟。
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime 是连接最长空闲时间；<=0 时使用 5 分钟。
	ConnMaxIdleTime time.Duration
}

// Open 打开数据库连接并完成连通性验证。
//
// 只建立连接池，不执行迁移；迁移请显式调用 Migrate。
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		return nil, fmt.Errorf("postgres: dsn is required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}

	maxOpen := cfg.MaxOpen
	if maxOpen <= 0 {
		maxOpen = 20
	}
	maxIdle := cfg.MaxIdle
	if maxIdle <= 0 {
		maxIdle = 10
	}
	lifetime := cfg.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = 30 * time.Minute
	}
	idleTime := cfg.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = 5 * time.Minute
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(lifetime)
	db.SetConnMaxIdleTime(idleTime)

	// 尽早暴露配置错误，而不是等第一次查询。
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return db, nil
}

// Migrate 按文件名顺序执行全部建表脚本。
//
// 脚本使用 IF NOT EXISTS，因此可重复执行（幂等）。
// 生产环境如需版本管理，可在此基础上记录已执行脚本。
func Migrate(ctx context.Context, db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("postgres: read migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("postgres: read %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("postgres: exec %s: %w", name, err)
		}
		slog.Info("postgres: migration applied", "file", name)
	}
	return nil
}
