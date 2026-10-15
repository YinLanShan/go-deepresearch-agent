package llm

import (
	"context"

	openai "github.com/cloudwego/eino-ext/components/model/openai"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
)

// newOpenAI 构造一个 OpenAI 兼容的 ChatModel。
//
// 依据：eino-examples/quickstart/chat/openai.go 使用
// openai.NewChatModel(ctx, &openai.ChatModelConfig{BaseURL, Model, APIKey})，
// 返回的 *openai.ChatModel 实现了 model.ToolCallingChatModel，
// 其 Stream 方法签名与 llm.ChatModel 一致。
func newOpenAI(ctx context.Context, cfg Config) (ChatModel, error) {
	m, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
		Model:   cfg.ModelName,
	})
	if err != nil {
		return nil, fault.Wrap(fault.CodeModelUnavailable, fault.LayerInfrastructure,
			"create openai chat model", err)
	}
	return m, nil
}
