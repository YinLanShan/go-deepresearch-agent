// Package config 负责从环境变量加载服务运行时配置，并提供默认值。
//
// 阶段 0 采用环境变量作为唯一配置来源，避免引入外部配置库。
package config

import (
	"os"
	"strings"
	"time"
)

// Config 是服务的运行时配置。
type Config struct {
	// HTTPAddr 是 Hertz 服务的监听地址，例如 ":8080"。
	HTTPAddr string
	// LogLevel 是日志级别，支持 debug/info/warn/error。
	LogLevel string

	// Model 是模型调用相关配置。
	Model ModelConfig

	// Tool 是 Supervisor（ReAct Agent）的工具配置。
	Tool ToolConfig

	// Execution 是 Scheduler / WorkPool 的执行资源与并发配置。
	Execution ExecutionConfig

	// Database 是持久化配置；默认禁用（用内存实现，便于本地开发）。
	Database DatabaseConfig
}

// DatabaseConfig 描述 PostgreSQL 持久化配置。
type DatabaseConfig struct {
	// Enabled 为 true 时使用 PostgreSQL，否则使用内存实现。
	Enabled bool
	// DSN 形如 postgres://user:pass@host:5432/db?sslmode=disable
	DSN string
	// MaxOpen 是最大打开连接数。
	MaxOpen int
	// MaxIdle 是最大空闲连接数。
	MaxIdle int
	// ConnMaxLifetime 是连接最长存活时间。
	ConnMaxLifetime time.Duration
	// Migrate 为 true 时启动自动建表（幂等）。
	Migrate bool
}

// ExecutionConfig 描述执行实体的并发能力。
type ExecutionConfig struct {
	// MaxConcurrency 是 Scheduler 同时调度的 Assignment 上限。
	MaxConcurrency int
	// Workers 是 WorkPool 的 Worker 数量（并发执行 Work 的能力）。
	Workers int
	// WorkTimeout 是单个 Work（子 Agent 一次执行）的超时。
	// 超时后 WorkPool 会取消该子 Agent 的 ctx，使其立刻停止。
	WorkTimeout time.Duration
}

// ModelConfig 描述模型提供方及其连接参数。
type ModelConfig struct {
	// Provider 是模型提供方，支持 "openai"（OpenAI 兼容接口）与 "mock"（进程内测试实现）。
	Provider string
	// BaseURL 是 OpenAI 兼容接口地址，为空时使用提供方默认地址。
	BaseURL string
	// APIKey 是模型鉴权密钥。
	APIKey string
	// ModelName 是模型名称。
	ModelName string
	// MaxRetries 是可重试错误（限流 / 服务不可用）的重试次数；0 表示不重试。
	MaxRetries int
	// RetryInterval 是重试的固定间隔；<=0 时由 llm 层使用默认值。
	RetryInterval time.Duration
}

// ToolConfig 描述工具来源与审批策略。
type ToolConfig struct {
	// MCPServersJSON 是 MCP Server 列表的 JSON 配置，结构见 internal/tool.MCPServerConfig。
	MCPServersJSON string
	// EnableBuiltinSearch 为 true 时注册内置演示搜索工具（无 MCP Server 也能跑通链路）。
	EnableBuiltinSearch bool
	// RequireApproval 是需要人工审批的工具名，逗号分隔；"*" 表示全部工具。
	//
	// 显式关闭审批：设为 "-" 或 "none"（不区分大小写），表示所有工具都不需要审批。
	// 留空等同于默认值 "*"，即全部需要审批——请勿用空值表达"关闭"。
	RequireApproval string
	// ApprovalTimeoutSeconds 是审批等待超时秒数，<=0 时使用默认值。
	ApprovalTimeoutSeconds int
}

// 审批开关的显式关闭标记。
const (
	// ApprovalOffDash 是"关闭审批"的简写标记。
	ApprovalOffDash = "-"
	// ApprovalOffNone 是"关闭审批"的语义化标记。
	ApprovalOffNone = "none"
)

// ApprovalRequired 解析 RequireApproval 并返回需要审批的工具名列表。
//
// 返回 nil 表示所有工具都不需要审批。
func (c ToolConfig) ApprovalRequired() []string {
	raw := strings.TrimSpace(c.RequireApproval)
	switch strings.ToLower(raw) {
	case "", ApprovalOffDash, ApprovalOffNone:
		if raw == "" {
			// 留空沿用默认：全部需要审批（默认值由 Load 填入 "*"，这里只处理被显式清空的情况）
			return []string{"*"}
		}
		return nil
	}

	out := make([]string, 0, 4)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Load 从环境变量读取配置，缺失时使用默认值。
func Load() Config {
	return Config{
		HTTPAddr: getenv("DRA_HTTP_ADDR", ":8080"),
		LogLevel: getenv("DRA_LOG_LEVEL", "info"),
		Model: ModelConfig{
			Provider:      getenv("DRA_MODEL_PROVIDER", "mock"),
			BaseURL:       getenv("DRA_OPENAI_BASE_URL", ""),
			APIKey:        getenv("DRA_OPENAI_API_KEY", ""),
			ModelName:     getenv("DRA_OPENAI_MODEL", "gpt-4o-mini"),
			MaxRetries:    getenvInt("DRA_MODEL_MAX_RETRIES", 2),
			RetryInterval: getenvSeconds("DRA_MODEL_RETRY_INTERVAL", 2*time.Second),
		},
		Tool: ToolConfig{
			MCPServersJSON:         getenv("DRA_MCP_SERVERS", ""),
			EnableBuiltinSearch:    getenv("DRA_ENABLE_BUILTIN_SEARCH", "true") != "false",
			RequireApproval:        getenv("DRA_TOOL_REQUIRE_APPROVAL", "*"),
			ApprovalTimeoutSeconds: getenvInt("DRA_APPROVAL_TIMEOUT_SECONDS", 300),
		},
		Execution: ExecutionConfig{
			MaxConcurrency: getenvInt("DRA_SCHEDULER_MAX_CONCURRENCY", 2),
			Workers:        getenvInt("DRA_WORKPOOL_WORKERS", 2),
			WorkTimeout:    getenvSeconds("DRA_WORK_TIMEOUT", 5*time.Minute),
		},
		Database: DatabaseConfig{
			Enabled:         getenvBool("DRA_DB_ENABLED", false),
			DSN:             getenv("DRA_DB_DSN", ""),
			MaxOpen:         getenvInt("DRA_DB_MAX_OPEN", 20),
			MaxIdle:         getenvInt("DRA_DB_MAX_IDLE", 10),
			ConnMaxLifetime: getenvSeconds("DRA_DB_CONN_MAX_LIFETIME", 30*time.Minute),
			Migrate:         getenvBool("DRA_DB_MIGRATE", true),
		},
	}
}

// getenvBool 读取布尔型环境变量；无法识别时返回默认值。
func getenvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func getenvInt(key string, def int) int {
	v := getenv(key, "")
	if v == "" {
		return def
	}
	var n int
	for _, r := range v {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// getenvSeconds 读取以秒为单位的时长（正整数），解析失败时使用默认值。
func getenvSeconds(key string, def time.Duration) time.Duration {
	n := getenvInt(key, -1)
	if n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
