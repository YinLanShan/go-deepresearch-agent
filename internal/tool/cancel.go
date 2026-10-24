package tool

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// CancelAssignmentsToolName 是"撤销研究子任务"工具的固定名称。
//
// 它让 Supervisor 在派发之后仍能纠正自己的决定：
//   - 允许范围：研究结束（finish_research）之前，模型可随时撤销；
//   - 撤销是即时的：槽位与 Work 都会立刻失效，此后到达的结果不被接受；
//   - 即使结果已经合法提交，也会从 ResearchState 中撤回（撤销也是一次提交动作）。
const CancelAssignmentsToolName = "cancel_assignments"

// CancelAssignmentsInput 是 cancel_assignments 的入参。
type CancelAssignmentsInput struct {
	// AssignmentIDs 是要撤销的 Assignment ID 列表（来自上下文中的子任务清单）。
	AssignmentIDs []string `json:"assignment_ids" jsonschema:"description=要撤销的 Assignment ID 列表，必须来自上下文中列出的子任务 ID"`
	// Reason 是撤销原因（用于提交历史与前端展示）。
	Reason string `json:"reason" jsonschema:"description=撤销原因，简要说明为什么这些子任务不再需要"`
}

// CancelAssignmentsOutput 是 cancel_assignments 的出参。
type CancelAssignmentsOutput struct {
	// Cancelled 是成功撤销的子任务数量。
	Cancelled int `json:"cancelled"`
	// RevokedResults 是从 ResearchState 中撤回的已提交结果条数。
	RevokedResults int `json:"revoked_results"`
	// Rejected 是未处理的条目说明（例如 ID 不存在、研究已结束等）。
	Rejected []string `json:"rejected,omitempty"`
}

// Canceller 由 SupervisorLoop 实现：接收"撤销子任务"的请求。
type Canceller interface {
	// CancelAssignments 撤销一批子任务，返回撤销数量、撤回的已提交结果条数与拒绝说明。
	CancelAssignments(ctx context.Context, assignmentIDs []string, reason string) (cancelled int, revoked int, rejected []string, err error)
}

// maxAssignmentIDsPerCall 是单次撤销的 ID 上限。
const maxAssignmentIDsPerCall = 32

// NewCancelAssignmentsTool 构造 cancel_assignments 工具。
func NewCancelAssignmentsTool(c Canceller) (tool.InvokableTool, error) {
	return utils.InferTool[CancelAssignmentsInput, CancelAssignmentsOutput](
		CancelAssignmentsToolName,
		"撤销此前派发的研究子任务，用于纠正派发决定。"+
			"当你判断某些子任务不再需要、目标写错了、或者与新的方向重复时调用。"+
			"撤销会立即生效：排队中的子任务被移除、执行中的被执行取消、"+
			"等待提交的结果会被拒绝；如果结果已经提交，也会从研究状态中撤回。"+
			"assignment_ids 必须使用上下文中列出的子任务 ID。",
		func(ctx context.Context, in CancelAssignmentsInput) (CancelAssignmentsOutput, error) {
			if c == nil {
				return CancelAssignmentsOutput{}, fmt.Errorf("canceller is not configured")
			}

			ids := make([]string, 0, len(in.AssignmentIDs))
			seen := make(map[string]struct{}, len(in.AssignmentIDs))
			var rejected []string
			for _, id := range in.AssignmentIDs {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				if len(ids) >= maxAssignmentIDsPerCall {
					rejected = append(rejected,
						fmt.Sprintf("超出单次撤销上限（%d 条），其余 ID 未处理", maxAssignmentIDsPerCall))
					break
				}
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				return CancelAssignmentsOutput{}, fmt.Errorf("assignment_ids is required")
			}

			reason := truncateRunes(strings.TrimSpace(in.Reason), maxGoalLength)
			cancelled, revoked, moreRejected, err := c.CancelAssignments(ctx, ids, reason)
			if err != nil {
				return CancelAssignmentsOutput{}, err
			}
			rejected = append(rejected, moreRejected...)
			return CancelAssignmentsOutput{
				Cancelled:      cancelled,
				RevokedResults: revoked,
				Rejected:       rejected,
			}, nil
		},
	)
}
