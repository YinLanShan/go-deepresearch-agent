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

	// SupervisorModel 是Supervisor内模型调用相关配置。
	SupervisorModel ModelConfig
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

// Load 从环境变量读取配置，缺失时使用默认值。
func Load() Config {
	return Config{
		HTTPAddr: getenv("DRA_HTTP_ADDR", ":8080"),
		LogLevel: getenv("DRA_LOG_LEVEL", "info"),
		SupervisorModel: ModelConfig{
			Provider:  getenv("DRA_MODEL_PROVIDER", "mock"),
			BaseURL:   getenv("DRA_OPENAI_BASE_URL", ""),
			APIKey:    getenv("DRA_OPENAI_API_KEY", ""),
			ModelName: getenv("DRA_OPENAI_MODEL", "gpt-4o-mini"),
		},
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}
