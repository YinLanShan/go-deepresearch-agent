// Package config 负责从环境变量加载服务运行时配置，并提供默认值。
//
// 阶段 0 采用环境变量作为唯一配置来源，避免引入外部配置库。
package config

import (
	"os"
	"strings"
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
}

// ToolConfig 描述工具来源与审批策略。
type ToolConfig struct {
	// MCPServersJSON 是 MCP Server 列表的 JSON 配置，结构见 internal/tool.MCPServerConfig。
	MCPServersJSON string
	// EnableBuiltinSearch 为 true 时注册内置演示搜索工具（无 MCP Server 也能跑通链路）。
	EnableBuiltinSearch bool
	// RequireApproval 是需要人工审批的工具名，逗号分隔；"*" 表示全部工具。
	RequireApproval string
	// ApprovalTimeoutSeconds 是审批等待超时秒数，<=0 时使用默认值。
	ApprovalTimeoutSeconds int
}

// Load 从环境变量读取配置，缺失时使用默认值。
func Load() Config {
	return Config{
		HTTPAddr: getenv("DRA_HTTP_ADDR", ":8080"),
		LogLevel: getenv("DRA_LOG_LEVEL", "info"),
		Model: ModelConfig{
			Provider:  getenv("DRA_MODEL_PROVIDER", "mock"),
			BaseURL:   getenv("DRA_OPENAI_BASE_URL", ""),
			APIKey:    getenv("DRA_OPENAI_API_KEY", ""),
			ModelName: getenv("DRA_OPENAI_MODEL", "gpt-4o-mini"),
		},
		Tool: ToolConfig{
			MCPServersJSON:         getenv("DRA_MCP_SERVERS", ""),
			EnableBuiltinSearch:    getenv("DRA_ENABLE_BUILTIN_SEARCH", "true") != "false",
			RequireApproval:        getenv("DRA_TOOL_REQUIRE_APPROVAL", "*"),
			ApprovalTimeoutSeconds: getenvInt("DRA_APPROVAL_TIMEOUT_SECONDS", 300),
		},
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
