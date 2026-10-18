package researchstate

import (
	"context"
	"errors"
)

// ErrNotFound 表示 Session 对应的研究状态不存在。
var ErrNotFound = errors.New("researchstate: not found")

// ErrNotAccepting 表示 ResearchState 已中止，不再接收新的 Commit。
var ErrNotAccepting = errors.New("researchstate: not accepting commits")

// Store 定义 ResearchState 的持久化能力。
//
// ResearchState 按 SessionID 组织（一个 Session 一份研究状态）。
// 其生命周期与 Session 绑定：仅 Session 级联删除时调用 Delete。
//
// Commit 是研究结果进入权威历史的受控入口：追加 Result（证据）与 Finding（结论），
// 清空已合并进 Finding 的被中断上下文，并自增版本号。
type Store interface {
	Create(ctx context.Context, s *State) error
	Get(ctx context.Context, sessionID string) (*State, error)
	Commit(ctx context.Context, sessionID string, in CommitInput) error
	AccumulateInterruptedContext(ctx context.Context, sessionID, text string) error
	// StopCommits 关闭提交门控：此后新的 Commit 返回 ErrNotAccepting，正在进行的 Commit 继续完成。
	StopCommits(ctx context.Context, sessionID string) error
	// ResumeCommits 重新开启提交门控（中止后恢复研究时调用）。
	ResumeCommits(ctx context.Context, sessionID string) error
	Delete(ctx context.Context, sessionID string) error
}
