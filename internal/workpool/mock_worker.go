package workpool

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
)

// MockWorker 是一个不接入真实模型的执行器。
//
// 它按固定阶段推进并产出确定性的研究产物，用于打通
// Scheduler → Work → ResultAggregator → ResearchState Commit 的最小链路。
// 阶段变化会通过 progress 回调上报，前端据此展示子 Agent 的实时进度。
type MockWorker struct {
	// stepDelay 是每个阶段之间的间隔（测试可调小）。
	stepDelay time.Duration
	seq       int64
}

// 编译期断言实现 Worker 接口。
var _ Worker = (*MockWorker)(nil)

// NewMockWorker 构造一个 MockWorker。stepDelay <= 0 时默认 120ms。
func NewMockWorker(stepDelay time.Duration) *MockWorker {
	if stepDelay <= 0 {
		stepDelay = 120 * time.Millisecond
	}
	return &MockWorker{stepDelay: stepDelay}
}

// ID 返回该 Worker 的标识（多 Worker 时区分执行者）。
func (m *MockWorker) ID() string {
	n := atomic.AddInt64(&m.seq, 1)
	return fmt.Sprintf("mock-worker-%d", (n-1)%4+1)
}

// mockStages 是 Mock 执行的固定阶段（对应"必要的调用信息"）。
var mockStages = []struct {
	stage  string
	detail string
}{
	{"planning", "拆分研究目标"},
	{"retrieving", "检索内部知识（mock）"},
	{"analyzing", "归纳证据并形成结论"},
	{"summarizing", "整理最终研究产物"},
}

// Run 执行一次 Work，返回最终研究产物。
func (m *MockWorker) Run(ctx context.Context, w *domain.Work, progress func(stage, detail string)) (string, error) {
	for _, st := range mockStages {
		progress(st.stage, st.detail)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.stepDelay):
		}
	}

	// 产出确定性的研究产物：包含 Assignment 目标，便于在 ResearchState 中追溯来源。
	goal := strings.TrimSpace(w.Goal)
	if goal == "" {
		goal = "未命名研究目标"
	}
	return fmt.Sprintf("[%s] 关于「%s」的调研结论（第 %d 次尝试）："+
		"该方向已收集到关键事实，可作为后续综合判断的证据。",
		shortID(w.ID), goal, w.Attempt,
	), nil
}

// shortID 返回 ID 的前 8 个字符（不足则原样返回）。
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
