package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// SubmitAssignmentsToolName 是"派发研究子任务"工具的固定名称。
//
// 它是 Supervisor 决定"研究什么"的唯一表达方式：
//   - 模型自行决定派发几个子任务、每个子任务的目标是什么；
//   - 派发是异步的：工具立即返回，子任务由 Scheduler/WorkPool 后台执行，
//     模型可以立刻结束本轮，结果提交后会唤醒新一轮；
//   - 不调用该工具就不会产生任何子任务。
const SubmitAssignmentsToolName = "submit_assignments"

// AssignmentSpec 是一条待派发的研究子任务描述。
type AssignmentSpec struct {
	// Goal 是这条子任务要完成的研究目标（自然语言，应可独立完成）。
	Goal string `json:"goal" jsonschema:"description=该子任务要完成的研究目标，需可独立完成"`
}

// SubmitAssignmentsInput 是 submit_assignments 的入参。
type SubmitAssignmentsInput struct {
	Assignments []AssignmentSpec `json:"assignments" jsonschema:"description=本批要派发的研究子任务列表"`
}

// SubmitAssignmentsOutput 是 submit_assignments 的出参。
type SubmitAssignmentsOutput struct {
	// Accepted 是成功派发的子任务数量。
	Accepted int `json:"accepted"`
	// Dispatched 为 true 表示已交给后台执行，结果稍后通过新一轮推理返回。
	Dispatched bool `json:"dispatched"`
	// Rejected 是未通过校验的条目说明（为空表示全部接受）。
	Rejected []string `json:"rejected,omitempty"`
}

// Assigner 由 SupervisorLoop 实现：接收"派发子任务"的请求。
type Assigner interface {
	// SubmitAssignments 校验并派发一批子任务，返回接受数量与拒绝说明。
	SubmitAssignments(ctx context.Context, goals []string) (accepted int, rejected []string, err error)
}

// maxAssignmentsPerCall 是单次调用允许派发的子任务上限，防止模型一次提交过多。
const maxAssignmentsPerCall = 8

// maxGoalLength 是单个子任务目标的最大字符数。
const maxGoalLength = 500

// NewSubmitAssignmentsTool 构造 submit_assignments 工具。
func NewSubmitAssignmentsTool(a Assigner) (tool.InvokableTool, error) {
	return utils.InferTool[SubmitAssignmentsInput, SubmitAssignmentsOutput](
		SubmitAssignmentsToolName,
		"派发一批研究子任务。每个子任务由一个独立的后台 Agent 执行，"+
			"你不必等待它们完成即可结束本轮；子任务结果提交后会唤醒你继续决策。"+
			"请给出具体、可独立完成的研究目标，避免与已完成的子任务重复。",
		func(ctx context.Context, in SubmitAssignmentsInput) (SubmitAssignmentsOutput, error) {
			if a == nil {
				return SubmitAssignmentsOutput{}, fmt.Errorf("assigner is not configured")
			}

			goals := make([]string, 0, len(in.Assignments))
			var rejected []string
			for _, spec := range in.Assignments {
				goal := strings.TrimSpace(spec.Goal)
				switch {
				case goal == "":
					rejected = append(rejected, "（空目标已忽略）")
				case len([]rune(goal)) > maxGoalLength:
					rejected = append(rejected,
						fmt.Sprintf("目标过长已忽略：%s", truncateRunes(goal, 30)))
				default:
					goals = append(goals, goal)
				}
			}

			if len(goals) > maxAssignmentsPerCall {
				for _, g := range goals[maxAssignmentsPerCall:] {
					rejected = append(rejected,
						fmt.Sprintf("超出单次上限 %d 已忽略：%s", maxAssignmentsPerCall, truncateRunes(g, 30)))
				}
				goals = goals[:maxAssignmentsPerCall]
			}
			if len(goals) == 0 {
				return SubmitAssignmentsOutput{
					Rejected: rejected,
				}, fmt.Errorf("no valid assignment goal provided")
			}

			accepted, moreRejected, err := a.SubmitAssignments(ctx, goals)
			rejected = append(rejected, moreRejected...)
			if err != nil {
				return SubmitAssignmentsOutput{Rejected: rejected}, err
			}
			return SubmitAssignmentsOutput{
				Accepted:   accepted,
				Dispatched: accepted > 0,
				Rejected:   rejected,
			}, nil
		},
	)
}

// truncateRunes 按字符截断，避免中文字符被切坏。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
