package workpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// 子 Agent 可用的工具白名单：控制类工具绝不允许。
//
// 子 Agent 只负责"完成一个研究目标"，不能派发子任务、不能撤销、
// 更不能宣布整个研究结束。
var subAgentForbiddenTools = map[string]struct{}{
	"submit_assignments": {},
	"cancel_assignments": {},
	"finish_research":    {},
}

// SubAgentConfig 是 SubAgentWorker 的构造参数。
type SubAgentConfig struct {
	// Model 与 Supervisor 共用同一个模型实例。
	Model model.BaseChatModel
	// Tools 是完整工具集；构造 Worker 时会按白名单过滤。
	Tools []tool.BaseTool
	// InstructionTemplate 用于生成子 Agent 的系统提示词；
	// 为空时使用 DefaultSubAgentInstruction。
	InstructionTemplate string
	// MaxIterations 是 ReAct 最大迭代轮数；<=0 时使用默认 20。
	MaxIterations int
}

// DefaultSubAgentInstruction 是子 Agent 的默认提示词模板。
//
// %s 会被替换为该 Work 的研究目标。
const DefaultSubAgentInstruction = `你是一名研究子 Agent，只负责完成下面这一项研究目标：

【研究目标】
%s

工作方式：
- 需要外部资料时调用可用工具检索；不要凭空编造事实。
- 只围绕上述目标开展工作，不要扩展到其他主题。
- 不要尝试派发子任务、撤销子任务或结束整项研究——这些不属于你的职责。
- 目标达成后，直接输出该目标的研究结论（纯文本，不要输出工具调用）。`

// SubAgentFactory 为每个 Work 创建一个独立的子 Agent Worker。
//
// 每次 NewWorker 都会新建 ChatModelAgent 与 AgentInput，
// 因此多个 Work 可以真正并行执行。
type SubAgentFactory struct {
	model         model.BaseChatModel
	tools         []tool.BaseTool
	instruction   string
	maxIterations int
	seq           int64
}

// 编译期断言。
var _ WorkerFactory = (*SubAgentFactory)(nil)

// NewSubAgentFactory 构造子 Agent 工厂。
//
// 工具会按白名单过滤：submit_assignments / cancel_assignments / finish_research
// 一律不注入子 Agent。
func NewSubAgentFactory(cfg SubAgentConfig) *SubAgentFactory {
	instruction := cfg.InstructionTemplate
	if strings.TrimSpace(instruction) == "" {
		instruction = DefaultSubAgentInstruction
	}
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = 20
	}
	return &SubAgentFactory{
		model:         cfg.Model,
		tools:         filterSubAgentTools(cfg.Tools),
		instruction:   instruction,
		maxIterations: maxIter,
	}
}

// filterSubAgentTools 过滤掉控制类工具。
func filterSubAgentTools(tools []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if _, forbidden := subAgentForbiddenTools[info.Name]; forbidden {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NewWorker 为一个 Work 创建独立的子 Agent 执行器。
//
// 注意：这里**不注入任何 ChatModelAgentMiddleware**（自然也没有审批中间件）。
// 人工审批只作用于 Supervisor 的工具调用；SubAgent 是一次性执行单元，
// 没有可恢复的轮次状态，因此不参与审批流程。
func (f *SubAgentFactory) NewWorker(ctx context.Context, w *domain.Work) (Worker, error) {
	if f.model == nil {
		return nil, errors.New("workpool: sub-agent model is not configured")
	}
	if w == nil {
		return nil, errors.New("workpool: work is required")
	}

	instruction := fmt.Sprintf(f.instruction, w.Goal)
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "sub-agent",
		Description: "research sub agent",
		Instruction: instruction,
		Model:       f.model,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: f.tools},
		},
		MaxIterations: f.maxIterations,
	})
	if err != nil {
		return nil, fmt.Errorf("workpool: create chat model agent: %w", err)
	}

	seq := atomic.AddInt64(&f.seq, 1)
	return &SubAgentWorker{
		agent:       agent,
		instruction: instruction,
		id:          fmt.Sprintf("sub-agent-%d", seq),
	}, nil
}

// SubAgentWorker 用单次任务型 ChatModelAgent 执行一个 Work。
//
// 与 SupervisorLoop 的区别：它不跨轮、不持久化、不等待审批。
// 一次 Run 跑完即结束，因此不需要 TurnLoop 的轮次与 checkpoint 机制。
type SubAgentWorker struct {
	agent       *adk.ChatModelAgent
	instruction string
	id          string
}

// 编译期断言。
var _ Worker = (*SubAgentWorker)(nil)

// ID 返回 Worker 标识。
func (s *SubAgentWorker) ID() string { return s.id }

// Run 执行子 Agent，并把全部 assistant 输出累积为待提交的 Result。
//
// 中断处理：
//   - ctx 取消（用户中止 / 撤销 / 超时）：立即 cancel 掉本次 run 并返回 ctx.Err()，
//     仅停止消费 Next() 不足以让 Agent 停下；
//   - 工具审批中断：子 Agent 不支持审批，直接中止并返回明确错误。
func (s *SubAgentWorker) Run(ctx context.Context, w *domain.Work,
	progress func(stage, detail string)) (string, error) {
	if s.agent == nil {
		return "", errors.New("workpool: sub-agent is not initialized")
	}

	progress("planning", "解析研究目标")

	// 子 Agent 的 ctx 必须可取消：停止消费事件流不会让 Agent 停止。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	iter := s.agent.Run(runCtx, &adk.AgentInput{
		Messages:        []*schema.Message{schema.UserMessage(w.Goal)},
		EnableStreaming: true,
	})

	var answer strings.Builder
	for {
		// 取消优先：一旦 ctx 结束就立刻 cancel 并返回，不再等下一个事件。
		select {
		case <-ctx.Done():
			cancel()
			return answer.String(), ctx.Err()
		default:
		}

		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			// 取消导致的错误按 ctx 状态归类。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return answer.String(), ctxErr
			}
			return answer.String(), ev.Err
		}

		// 工具审批中断：子 Agent 不参与审批流程，直接中止。
		if ev.Action != nil && ev.Action.Interrupted != nil {
			cancel()
			return answer.String(), ErrSubAgentNeedsApproval
		}

		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput

		// 工具消息不是模型输出，跳过。
		if mv.Role == schema.Tool {
			progress("analyzing", "整合工具结果")
			continue
		}
		if mv.Role != schema.Assistant {
			continue
		}

		if mv.IsStreaming {
			if err := consumeMessageStream(mv.MessageStream, &answer, progress); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return answer.String(), ctxErr
				}
				return answer.String(), err
			}
			continue
		}
		if mv.Message != nil && mv.Message.Content != "" {
			answer.WriteString(mv.Message.Content)
		}
	}

	// 再检查一次：可能在最后一次 Next() 之后被取消。
	if err := ctx.Err(); err != nil {
		return answer.String(), err
	}

	result := strings.TrimSpace(answer.String())
	if result == "" {
		return "", ErrSubAgentEmptyResult
	}
	progress("summarizing", "整理最终研究产物")
	return result, nil
}

// ErrSubAgentNeedsApproval 表示子 Agent 上报了中断事件。
//
// 子 Agent 不注入审批中间件，正常情况下不会产生审批中断；
// 因此该错误只作为防御性兜底——一旦出现，说明有非预期组件触发了中断，
// 直接终止并把原因回告 Supervisor，不进入任何审批流程。
var ErrSubAgentNeedsApproval = errors.New("workpool: sub-agent was interrupted unexpectedly (approval is not supported)")

// ErrSubAgentEmptyResult 表示子 Agent 没有产出任何内容。
var ErrSubAgentEmptyResult = errors.New("workpool: sub-agent produced no output")

// consumeMessageStream 读取流式消息并累积到 answer。
func consumeMessageStream(sr *schema.StreamReader[*schema.Message], answer *strings.Builder,
	progress func(stage, detail string)) error {
	if sr == nil {
		return nil
	}
	defer sr.Close()

	firstChunk := true
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err // 流被取消（context）或出错，交由上层按 ctx 状态归类
		}
		if chunk == nil || chunk.Content == "" {
			continue
		}
		if firstChunk {
			progress("retrieving", "生成研究结论")
			firstChunk = false
		}
		answer.WriteString(chunk.Content)
	}
}
