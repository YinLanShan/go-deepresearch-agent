package tool

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// isInterruptRerun 判断错误是否为框架的中断重入错误。
// 这类错误必须原样返回，否则审批恢复链路会被破坏。
func isInterruptRerun(err error) bool {
	_, ok := compose.IsInterruptRerunError(err)
	return ok
}

// ApprovalInfo 是中断发生时交给前端展示的载荷。
//
// 注意：该类型会作为 any 存入 Eino 的 Interrupt 状态，并在 Checkpoint 序列化时
// 经过 gob 编码，因此必须在 init 中注册；否则中断会因
// "gob: type not registered for interface" 而无法保存 Checkpoint。
type ApprovalInfo struct {
	ToolName        string
	ArgumentsInJSON string
}

// ApprovalResult 是用户审批结果，作为 ResumeParams.Targets 的值传回工具。
// 同样需要在 gob 中注册。
type ApprovalResult struct {
	Approved bool
	Reason   string
}

func init() {
	// gob 只允许一个类型注册一个名字，因此只注册值类型，并在代码中统一使用值。
	schema.RegisterName[ApprovalInfo]("dra_tool_approval_info")
	schema.RegisterName[ApprovalResult]("dra_tool_approval_result")
}

// Recorder 用于把工具动作上报给业务层（前端展示）。
//
// 展示逻辑放在中间件里，因此 Recorder 由 SupervisorLoop 实现。
type Recorder interface {
	// ToolStarted 在工具真正执行前调用。
	ToolStarted(name, arguments string)
	// ToolFinished 在工具执行后调用。
	ToolFinished(name, output string, err error)
}

// NewApprovalMiddleware 构造"工具审批"中间件。
//
// 实现方式遵循 Eino 官方示例：需要审批的工具在首次执行时调用 tool.StatefulInterrupt
// 暂停整轮执行，由 ADK 序列化 Checkpoint；用户决定后通过 ResumeParams 恢复，
// 中间件从 resume 上下文取出决定，批准才真正执行工具。
//
// 本中间件不阻塞等待，也不自行实现中断/恢复。
func NewApprovalMiddleware(policy Policy) adk.ChatModelAgentMiddleware {
	return &approvalMiddleware{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		policy:                       policy,
	}
}

type approvalMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	policy Policy
}

// WrapInvokableToolCall 在需要审批的工具外包裹中断逻辑。
func (m *approvalMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	if tCtx == nil || !m.policy.NeedsApproval(tCtx.Name) {
		return endpoint, nil
	}

	return func(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
		// 首次执行：保存参数并中断，等待人工审批。
		wasInterrupted, _, storedArgs := tool.GetInterruptState[string](ctx)
		if !wasInterrupted {
			return "", tool.StatefulInterrupt(ctx, ApprovalInfo{
				ToolName:        tCtx.Name,
				ArgumentsInJSON: argumentsInJSON,
			}, argumentsInJSON)
		}

		// 恢复执行：取出用户决定。
		isTarget, hasData, data := tool.GetResumeContext[ApprovalResult](ctx)
		if isTarget && hasData {
			if data.Approved {
				return endpoint(ctx, storedArgs, opts...)
			}
			if data.Reason != "" {
				return fmt.Sprintf("tool %q was rejected by user: %s", tCtx.Name, data.Reason), nil
			}
			return fmt.Sprintf("tool %q was rejected by user", tCtx.Name), nil
		}

		// 恢复数据不属于本工具：保持中断，避免误执行。
		return "", tool.StatefulInterrupt(ctx, ApprovalInfo{
			ToolName:        tCtx.Name,
			ArgumentsInJSON: storedArgs,
		}, storedArgs)
	}, nil
}

// NewSafeToolMiddleware 构造"工具错误兜底"中间件。
//
// 把工具执行错误转换成可读文本回传给模型，避免单个工具失败中断整轮研究；
// 但中断重入错误（InterruptRerunError）必须原样抛给框架，否则会破坏审批恢复。
func NewSafeToolMiddleware() adk.ChatModelAgentMiddleware {
	return &safeToolMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

type safeToolMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

// WrapInvokableToolCall 把普通工具错误转为文本结果。
func (m *safeToolMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	_ *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
		out, err := endpoint(ctx, argumentsInJSON, opts...)
		if err != nil {
			if isInterruptRerun(err) {
				return "", err
			}
			return fmt.Sprintf("[tool error] %v", err), nil
		}
		return out, nil
	}, nil
}

// NewEventMiddleware 构造"工具动作上报"中间件，供前端展示调用与结果。
func NewEventMiddleware(rec Recorder) adk.ChatModelAgentMiddleware {
	return &eventMiddleware{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		recorder:                     rec,
	}
}

type eventMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	recorder Recorder
}

// WrapInvokableToolCall 在工具执行前后上报事件。
func (m *eventMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	name := ""
	if tCtx != nil {
		name = tCtx.Name
	}
	return func(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
		if m.recorder != nil {
			m.recorder.ToolStarted(name, argumentsInJSON)
		}
		out, err := endpoint(ctx, argumentsInJSON, opts...)
		if m.recorder != nil {
			m.recorder.ToolFinished(name, out, err)
		}
		return out, err
	}, nil
}
