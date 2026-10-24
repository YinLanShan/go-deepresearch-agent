package researchstate

import (
	"context"
	"errors"
)

// ErrNotFound 表示目标研究状态不存在。
var ErrNotFound = errors.New("researchstate: not found")

// ErrNotAccepting 表示 ResearchState 已暂停，不再接收新的 Commit。
var ErrNotAccepting = errors.New("researchstate: not accepting commits")

// Store 定义 ResearchState 的持久化能力。
//
// ResearchState 按 TaskID 组织（一个 Task 一份研究状态，跨 RunTask 重建存活）。
// 其生命周期与 RunTask 绑定：RunTask 创建时初始化；Task 对应的研究结束/中止后
// 保留为历史；仅 Session 级联删除时调用 DeleteBySession。
//
// Commit 追加 Result（证据）与 Finding（过程输出）并自增版本号，
// 同时清空已并入本轮 Finding 的 InterruptedContext。
// Conclusion 只由 finish_research 通过 SetConclusion 写入。
type Store interface {
	Create(ctx context.Context, s *State) error
	Get(ctx context.Context, taskID string) (*State, error)
	// GetLatestBySession 返回指定 Session 下最新序号的 ResearchState。
	GetLatestBySession(ctx context.Context, sessionID string) (*State, error)
	// ListBySession 返回指定 Session 下全部 ResearchState，按序号升序。
	ListBySession(ctx context.Context, sessionID string) ([]*State, error)
	// Commit 追加一轮 Result + Finding，并清空 InterruptedContext。
	Commit(ctx context.Context, taskID string, in CommitInput) error
	// AppendResult 仅追加一条 Result（用于任务中止后到达的迟到产物），
	// 不产生 Finding、不触碰 InterruptedContext。
	AppendResult(ctx context.Context, taskID string, in CommitInput) error
	AccumulateInterruptedContext(ctx context.Context, taskID, text string) error
	// AddRequirement 追加一条用户需求（按时间顺序累积，复数语义）。
	AddRequirement(ctx context.Context, taskID, requirement string) error
	// SetConclusion 写入该研究节点的最终结论（仅由 finish_research 调用）。
	SetConclusion(ctx context.Context, taskID, conclusion string) error
	// AppendMessage 追加一条对话历史（用于前端切换会话时回放）。
	AppendMessage(ctx context.Context, taskID string, role MessageRole, content string) error
	// StopCommits 关闭提交门控：此后新的 Commit 返回 ErrNotAccepting，正在进行的 Commit 继续完成。
	StopCommits(ctx context.Context, taskID string) error
	// ResumeCommits 重新开启提交门控（被挤掉的 RunTask 恢复运行时调用）。
	ResumeCommits(ctx context.Context, taskID string) error
	DeleteBySession(ctx context.Context, sessionID string) error
}
