// Package mock 提供进程内确定性的 ChatModel 实现，用于测试与无密钥联调。
package mock

import (
	"context"
	"fmt"
	"strings"
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
// 行为：输入中没有工具结果时，先发起一次工具调用；
// 已看到工具结果后，输出最终研究结论。
//
// 工具选择策略：优先调用 PreferTools 中第一个"模型实际可用"的工具，
// 这样换成真实 MCP Server 时也能直接复用（MCP 工具名与内置不同）。
type ToolModel struct {
	// ToolName 是指定的工具名；为空则按 PreferTools 从可用工具中挑选。
	ToolName string
	// PreferTools 是按优先级排列的候选工具名。
	PreferTools []string
	// Arguments 是工具调用参数（JSON）；默认 {"query":"框架执行模型"}。
	Arguments string
	// CallFinish 为 true 时，出结论后会调用 finish_research 宣布研究结束。
	CallFinish bool
	// CallAssign 为 true 时，首步调用 submit_assignments 派发研究子任务。
	CallAssign bool
	// AssignGoals 是 CallAssign 时要派发的子任务目标；为空时按需求派生默认目标。
	AssignGoals []string
}

// defaultPreferTools 是默认的工具优先级列表：
// 先试内置 web_search，再试 MCP 测试服务器的 echo/add。
var defaultPreferTools = []string{"web_search", "echo", "add"}

// NewToolModel 构造一个会发起工具调用的模型。
func NewToolModel() *ToolModel {
	return &ToolModel{
		PreferTools: defaultPreferTools,
		Arguments:   `{"query":"框架执行模型"}`,
	}
}

// selectTool 从模型可见的工具信息中挑出要调用的工具。
//
// available 为空（模型未收到工具信息）时退回默认首选名，
// 以便在没有工具信息的纯单测场景下仍能构造确定的 ToolCall。
func (m *ToolModel) selectTool(available []*schema.ToolInfo) (name, args string) {
	args = m.Arguments
	if args == "" {
		args = `{"query":"框架执行模型"}`
	}
	if m.ToolName != "" {
		return m.ToolName, args
	}

	// 按优先列表挑选实际可用的工具。
	for _, want := range m.PreferTools {
		for _, info := range available {
			if info != nil && info.Name == want {
				return want, argsFor(want, args)
			}
		}
	}

	// 优先列表都没命中：用模型实际拿到的第一个工具。
	for _, info := range available {
		if info != nil && info.Name != "" {
			return info.Name, argsFor(info.Name, args)
		}
	}

	return "web_search", args
}

// argsFor 按工具名给出合理的默认参数（不同工具的入参 schema 不同）。
func argsFor(toolName, fallback string) string {
	switch toolName {
	case "echo":
		return `{"text":"hello from mock model"}`
	case "add":
		return `{"a":17,"b":25}`
	case finishToolName:
		return `{"conclusion":"（mock）研究目标已达成，宣布结束。"}`
	default:
		return fallback
	}
}

// Generate 按"派发子任务 → 检索 → 出结论 → 宣布结束"的顺序推进。
//
// 可配置能力（默认全部关闭，保持既有测试行为稳定）：
//   - CallAssign：首步调用 submit_assignments 派发子任务；
//   - CallFinish：出结论后调用 finish_research 宣布研究结束。
func (m *ToolModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	cfg := model.GetCommonOptions(&model.Options{}, opts...)
	var available []*schema.ToolInfo
	if cfg != nil {
		available = cfg.Tools
	}

	// 统计已发生的工具调用，决定当前处于流程哪一步。
	//
	// 注意：Supervisor 被 résultat 唤醒时会以新输入开启一轮（历史里可能没有
	// 上一轮的工具消息），因此还要扫描 assistant 消息里的 ToolCalls，
	// 否则会重复派发同一批子任务，形成无限扩增。
	var lastToolResult string
	var sawAssignResult, sawFinishResult, assignedBefore bool
	for _, msg := range input {
		if msg == nil {
			continue
		}
		if msg.Role == schema.Assistant {
			for _, tc := range msg.ToolCalls {
				switch tc.Function.Name {
				case submitAssignmentsToolName:
					assignedBefore = true
				case finishToolName:
					sawFinishResult = true
				}
			}
			continue
		}
		if msg.Role != schema.Tool {
			continue
		}
		lastToolResult = msg.Content
		switch msg.ToolName {
		case submitAssignmentsToolName:
			sawAssignResult = true
		case finishToolName:
			sawFinishResult = true
		}
	}
	// 已经派发过子任务（无论工具结果是否还在历史里）就不再重复派发。
	alreadyAssigned := sawAssignResult || assignedBefore

	// 调用过 finish_research 之后：输出最终回答，不再发起工具调用。
	if sawFinishResult {
		return schema.AssistantMessage("研究已完成，以上为本次研究的最终结论。", nil), nil
	}

	// 首步：派发子任务（若配置且工具可用，且此前未派发过）。
	if m.CallAssign && !alreadyAssigned && hasTool(available, submitAssignmentsToolName) {
		msg := schema.AssistantMessage("我先拆分出需要并行推进的研究子任务。", nil)
		msg.ToolCalls = []schema.ToolCall{{
			ID:   "call-assign",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      submitAssignmentsToolName,
				Arguments: m.assignArguments(),
			},
		}}
		return msg, nil
	}

	// 已派发过子任务：不再检索，直接出结论；配置了 CallFinish 则顺便宣布结束。
	if alreadyAssigned {
		conclusion := "已把研究拆分给后台子任务并汇总其结论。"
		if lastToolResult != "" {
			conclusion = "根据检索结果，可以得出结论：" + lastToolResult
		}
		if m.CallFinish && hasTool(available, finishToolName) {
			msg := schema.AssistantMessage(conclusion+" 研究到此结束。", nil)
			msg.ToolCalls = []schema.ToolCall{{
				ID:   "call-finish",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      finishToolName,
					Arguments: `{"conclusion":"` + escapeJSON(conclusion) + `"}`,
				},
			}}
			return msg, nil
		}
		return schema.AssistantMessage(conclusion, nil), nil
	}

	// 已有检索结果：出结论；若配置了 CallFinish，则再调用一次 finish_research 宣布结束。
	if lastToolResult != "" {
		conclusion := "根据检索结果，可以得出结论：" + lastToolResult
		if m.CallFinish && hasTool(available, finishToolName) {
			msg := schema.AssistantMessage(conclusion+" 研究到此结束。", nil)
			msg.ToolCalls = []schema.ToolCall{{
				ID:   "call-finish",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      finishToolName,
					Arguments: `{"conclusion":"` + escapeJSON(conclusion) + `"}`,
				},
			}}
			return msg, nil
		}
		return schema.AssistantMessage(conclusion, nil), nil
	}

	name, args := m.selectTool(available)
	msg := schema.AssistantMessage("我需要先检索相关资料。", nil)
	msg.ToolCalls = []schema.ToolCall{{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      name,
			Arguments: args,
		},
	}}
	return msg, nil
}

// finishToolName 与 internal/tool.FinishToolName 保持一致（避免反向依赖）。
const finishToolName = "finish_research"

// submitAssignmentsToolName 与 internal/tool.SubmitAssignmentsToolName 保持一致。
const submitAssignmentsToolName = "submit_assignments"

// assignArguments 构造 submit_assignments 的调用参数。
func (m *ToolModel) assignArguments() string {
	goals := m.AssignGoals
	if len(goals) == 0 {
		goals = []string{
			"收集与该主题相关的基础事实",
			"分析该主题的关键权衡与约束",
		}
	}
	parts := make([]string, 0, len(goals))
	for _, g := range goals {
		parts = append(parts, `{"goal":"`+escapeJSON(g)+`"}`)
	}
	return `{"assignments":[` + strings.Join(parts, ",") + `]}`
}

// hasTool 判断模型可见工具里是否包含指定名称。
func hasTool(available []*schema.ToolInfo, name string) bool {
	for _, info := range available {
		if info != nil && info.Name == name {
			return true
		}
	}
	return false
}

// escapeJSON 转义字符串中的双引号与换行，便于拼装简单 JSON。
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
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
