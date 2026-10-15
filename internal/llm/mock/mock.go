// Package mock 提供进程内确定性的 ChatModel 实现，用于测试与无密钥联调。
package mock

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// chunkSize 控制流式回复每块的字符数，便于测试断言。
const chunkSize = 8

// Model 是确定性的 ChatModel，回显最后一条用户消息。
type Model struct{}

// New 构造一个 mock Model。
func New() *Model { return &Model{} }

// 编译期断言实现 eino 的完整基础模型接口。
var _ model.BaseChatModel = (*Model)(nil)

// Generate 返回一条完整回复。
func (m *Model) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(m.reply(input), nil), nil
}

// Stream 返回由多块构成的确定性回复流。
func (m *Model) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reply := m.reply(input)
	var chunks []*schema.Message
	for i := 0; i < len(reply); i += chunkSize {
		end := i + chunkSize
		if end > len(reply) {
			end = len(reply)
		}
		chunks = append(chunks, schema.AssistantMessage(reply[i:end], nil))
	}
	if len(chunks) == 0 {
		chunks = append(chunks, schema.AssistantMessage("", nil))
	}
	return schema.StreamReaderFromArray(chunks), nil
}

func (m *Model) reply(input []*schema.Message) string {
	var last string
	for _, msg := range input {
		if msg != nil && msg.Role == schema.User {
			last = msg.Content
		}
	}
	if last == "" {
		return "mock reply (no user message)"
	}
	return fmt.Sprintf("mock reply to: %s", last)
}
