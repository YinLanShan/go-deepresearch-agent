// Package mock 提供进程内确定性的 ChatModel 实现，用于测试与无密钥联调。
package mock

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// chunkSize 控制流式回复每块的字符数，便于测试断言。
const chunkSize = 8

// Model 是确定性的 ChatModel，回显最后一条用户消息。
type Model struct{}

// New 构造一个 mock Model。
func New() *Model { return &Model{} }

// SlowModel 是慢速模型，用于测试中断/抢占场景。
// Generate 阻塞 Delay 后返回；Stream 逐块输出，块间有 Delay，且响应 ctx 取消。
type SlowModel struct {
	Delay     time.Duration
	ChunkSize int
}

// NewSlowModel 构造一个慢速模型。
func NewSlowModel(delay time.Duration) *SlowModel {
	return &SlowModel{Delay: delay, ChunkSize: chunkSize}
}

// 编译期断言实现 eino 的完整基础模型接口。
var (
	_ model.BaseChatModel = (*Model)(nil)
	_ model.BaseChatModel = (*SlowModel)(nil)
	_ model.BaseChatModel = (*ToolModel)(nil)
)

// ToolModel 是用于验证 ReAct 工具调用链路的确定性模型。
//
// 行为：输入中没有工具结果时，先发起一次 web_search 工具调用；
// 已看到工具结果后，输出最终研究结论。
type ToolModel struct {
	// ToolName 是要调用的工具名，默认 web_search。
	ToolName string
	// Arguments 是工具调用参数（JSON），默认 {"query":"框架执行模型"}。
	Arguments string
}

// NewToolModel 构造一个会发起工具调用的模型。
func NewToolModel() *ToolModel {
	return &ToolModel{
		ToolName:  "web_search",
		Arguments: `{"query":"框架执行模型"}`,
	}
}

// Generate 首次发起工具调用，看到工具结果后输出最终回答。
func (m *ToolModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	for _, msg := range input {
		if msg != nil && msg.Role == schema.Tool {
			return schema.AssistantMessage(
				"根据检索结果，可以得出结论：该框架采用 DAG 执行模型，适合确定性 Workflow。", nil), nil
		}
	}

	msg := schema.AssistantMessage("我需要先检索相关资料。", nil)
	msg.ToolCalls = []schema.ToolCall{{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      m.ToolName,
			Arguments: m.Arguments,
		},
	}}
	return msg, nil
}

// Stream 以单块流返回与 Generate 相同的内容。
func (m *ToolModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// Generate 返回一条完整回复。
func (m *Model) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(reply(input), nil), nil
}

// Stream 返回由多块构成的确定性回复流。
func (m *Model) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := splitChunks(reply(input), chunkSize)
	return schema.StreamReaderFromArray(toMessages(chunks)), nil
}

// Generate 阻塞 Delay 后返回完整回复，响应 ctx 取消。
func (m *SlowModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if err := sleep(ctx, m.Delay); err != nil {
		return nil, err
	}
	return schema.AssistantMessage(reply(input), nil), nil
}

// Stream 逐块输出回复，块间有 Delay，响应 ctx 取消（用于测试中断抢占）。
func (m *SlowModel) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := splitChunks(reply(input), m.ChunkSize)
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	sr, sw := schema.Pipe[*schema.Message](len(chunks))
	go func() {
		defer sw.Close()
		for _, c := range chunks {
			if err := sleep(ctx, m.Delay); err != nil {
				sw.Send(nil, err)
				return
			}
			if sw.Send(schema.AssistantMessage(c, nil), nil) {
				return // 流已关闭
			}
		}
	}()
	return sr, nil
}

// reply 回显最后一条用户消息。
func reply(input []*schema.Message) string {
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

func splitChunks(s string, size int) []string {
	if size <= 0 {
		size = chunkSize
	}
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func toMessages(chunks []string) []*schema.Message {
	msgs := make([]*schema.Message, 0, len(chunks))
	for _, c := range chunks {
		msgs = append(msgs, schema.AssistantMessage(c, nil))
	}
	if len(msgs) == 0 {
		msgs = append(msgs, schema.AssistantMessage("", nil))
	}
	return msgs
}

// sleep 阻塞 d，响应 ctx 取消。
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
