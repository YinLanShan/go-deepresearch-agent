package tool

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// FinishToolName 是"研究结束"工具的固定名称。
//
// 它是唯一能让研究进入终结态的模型动作：
//   - 只有模型显式调用它，RunTask 才会置为 finished；
//   - 调用后不可撤销（不允许模型"反悔"）；
//   - 调用时无论是否仍有 Assignment 在运行或等待提交，一律取消。
const FinishToolName = "finish_research"

// Finisher 由 SupervisorLoop 实现：接收"结束研究"的请求。
type Finisher interface {
	// RequestFinish 记录终结意图；conclusion 为可选的研究结论总结。
	RequestFinish(conclusion string)
}

// FinishResearchInput 是 finish_research 的入参。
type FinishResearchInput struct {
	// Conclusion 是整项研究的最终结论，不是本轮小结。
	// 它会被单独写入 ResearchState.Conclusion，作为该研究节点的结论。
	Conclusion string `json:"conclusion" jsonschema:"description=整项研究的最终结论（不是本轮小结）：应总结研究目标是什么、得出了哪些关键结论、还有哪些未解决的限制。该结论会作为本次研究的正式结论长期保存。"`
}

// FinishResearchOutput 是 finish_research 的出参。
type FinishResearchOutput struct {
	Status string `json:"status"`
}

// NewFinishResearchTool 构造 finish_research 工具。
//
// 该工具只负责"标记终结意图"；真正的终止流程（写入 Conclusion、取消后台任务、
// 关闭提交门控、置 RunTask 为 finished）由 SupervisorLoop 在本轮收尾时统一执行，
// 以保证"模型调用后不会再被新一轮推理推翻"。
func NewFinishResearchTool(f Finisher) (tool.InvokableTool, error) {
	return utils.InferTool[FinishResearchInput, FinishResearchOutput](
		FinishToolName,
		"宣布本次研究结束，并给出整项研究的最终结论。"+
			"当研究目标已达成、没有必须继续的工作时调用。"+
			"conclusion 必须是针对整个研究目标的总结（而非本轮工作的小结），"+
			"因为它是该研究节点的正式结论，会长期保留并传递给后续研究节点。"+
			"调用后不可撤销，且会取消所有仍在运行或等待提交的后台研究任务。",
		func(_ context.Context, in FinishResearchInput) (FinishResearchOutput, error) {
			if f == nil {
				return FinishResearchOutput{}, fmt.Errorf("finisher is not configured")
			}
			f.RequestFinish(in.Conclusion)
			return FinishResearchOutput{Status: "finishing"}, nil
		},
	)
}
