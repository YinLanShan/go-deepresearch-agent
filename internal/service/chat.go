// Package service 是 Application Layer 的业务入口。
//
// 对应 docs/Architecture.md 6.2 节 Application Layer 的定位：负责业务编排，
// 不直接参与具体推理。阶段 0 提供 ChatService，将用户消息交给模型并流式回传。
package service

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
)

const systemPrompt = "You are a helpful research assistant."

// ChatService 是阶段 0 的最小应用服务。
type ChatService struct {
	model llm.ChatModel
}

// NewChatService 构造一个 ChatService。
func NewChatService(m llm.ChatModel) *ChatService {
	return &ChatService{model: m}
}

// Stream 将用户消息交给模型，并通过 fn 逐块回调模型输出。
// fn 返回错误（例如下游 SSE 写入失败）时立即中止流式消费并向上返回。
func (s *ChatService) Stream(ctx context.Context, userMessage string, fn func(delta string) error) error {
	messages := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(userMessage),
	}

	stream, err := s.model.Stream(ctx, messages)
	if err != nil {
		return fault.Wrap(fault.CodeModelUnavailable, fault.LayerInfrastructure, "model stream", err)
	}
	defer stream.Close()

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fault.Wrap(fault.CodeModelUnavailable, fault.LayerInfrastructure, "model recv", err)
		}
		if chunk == nil {
			continue
		}
		if err := fn(chunk.Content); err != nil {
			return err
		}
	}
}
