// Package llm 封装模型调用能力，是 Infrastructure 层的一部分。
//
// 阶段 0 只暴露流式对话补全能力。ChatModel 接口的方法签名与
// eino 的 model.BaseChatModel.Stream 完全一致，因此真实实现
// （eino-ext 的 openai.ChatModel）无需适配即可直接复用。
package llm

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
)

// ChatModel 是阶段 0 所需的最小模型能力：流式对话补全。
type ChatModel interface {
	Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// Config 描述模型提供方及其连接参数。
type Config struct {
	Provider  string
	BaseURL   string
	APIKey    string
	ModelName string
}

// New 根据配置构造一个 ChatModel。
//
// Provider 支持：
//   - "mock"：进程内确定性实现，用于本地联调与测试，无需密钥。
//   - "openai"：OpenAI 兼容接口，可指向 OpenAI / DeepSeek 等兼容服务。
func New(ctx context.Context, cfg Config) (ChatModel, error) {
	switch cfg.Provider {
	case "mock":
		return mock.New(), nil
	case "openai":
		return newOpenAI(ctx, cfg)
	default:
		return nil, fault.New(fault.CodeInvalidInput, fault.LayerInfrastructure,
			fmt.Sprintf("unsupported model provider %q", cfg.Provider))
	}
}
