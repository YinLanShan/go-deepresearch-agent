// Package llm 封装模型调用能力，是 Infrastructure 层的一部分。
//
// 阶段 0 只暴露流式对话补全能力。ChatModel 接口的方法签名与
// eino 的 model.BaseChatModel.Stream 完全一致，因此真实实现
// （eino-ext 的 openai.ChatModel）无需适配即可直接复用。
package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
)

// ChatModel 是项目统一的模型能力抽象，嵌入 eino 的 model.BaseChatModel，
// 同时具备 Generate（非流式）与 Stream（流式）两种能力，
// 供 ChatService（流式输出）与 SupervisorLoop（ADK，需 Generate）复用。
type ChatModel interface {
	model.BaseChatModel
}

// Config 描述模型提供方及其连接参数。
type Config struct {
	Provider  string
	BaseURL   string
	APIKey    string
	ModelName string
}

// openai 提供方在配置不全时使用的默认值。
//
// 目的是"配置不全不应导致模型不可用"：BaseURL / ModelName 留空时自动回退到默认值，
// 前端也据此展示占位提示。
const (
	// DefaultBaseURL 是默认的 OpenAI 兼容接口地址（DeepSeek 兼容端点）。
	DefaultBaseURL = "https://api.deepseek.com"
	// DefaultModelName 是默认模型名。
	DefaultModelName = "deepseek-chat"
)

// WithDefaults 为可留空字段填充默认值。
//
//   - Provider 留空 → mock（进程内确定性模型）；
//   - openai 的 BaseURL 留空 → DefaultBaseURL；
//   - openai 的 ModelName 留空 → DefaultModelName。
func (c Config) WithDefaults() Config {
	out := c
	out.Provider = strings.TrimSpace(out.Provider)
	out.BaseURL = strings.TrimSpace(out.BaseURL)
	out.ModelName = strings.TrimSpace(out.ModelName)

	if out.Provider == "" {
		out.Provider = "mock"
	}
	if out.Provider == "openai" {
		if out.BaseURL == "" {
			out.BaseURL = DefaultBaseURL
		}
		if out.ModelName == "" {
			out.ModelName = DefaultModelName
		}
	}
	return out
}

// New 根据配置构造一个 ChatModel。
//
// Provider 支持：
//   - "mock"：进程内确定性实现，用于本地联调与测试，无需密钥。
//   - "mock-tool"：确定性实现，会发起一次 web_search 工具调用，用于验证 ReAct + 审批链路。
//   - "openai"：OpenAI 兼容接口，可指向 OpenAI / DeepSeek 等兼容服务。
func New(ctx context.Context, cfg Config) (ChatModel, error) {
	cfg = cfg.WithDefaults()

	switch cfg.Provider {
	case "mock":
		return mock.New(), nil
	case "mock-tool":
		return mock.NewToolModel(), nil
	case "openai":
		return newOpenAI(ctx, cfg)
	default:
		return nil, fault.New(fault.CodeInvalidInput, fault.LayerInfrastructure,
			fmt.Sprintf("unsupported model provider %q", cfg.Provider))
	}
}
