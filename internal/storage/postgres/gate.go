package postgres

import "sync"

// commitGate 是提交门控的进程内实现。
//
// 为什么门控不落库（docs/DataModel.md 4.4）：
//   - 门控是派生状态：它表达"该 Task 现在允许提交吗"，
//     而这个答案由 RunTask 状态决定，无需持久化；
//   - 进程重启后所有 RunTask 要么已终止、要么需恢复，
//     存了反而要处理"重启后默认开还是关"的歧义；
//   - 内存实现同样是内存态，保持一致。
//
// 语义：默认放行（未记录的 Task 视为开启），
// 这与内存实现的"Create 时显式开启 + 缺省即未知"一致，
// 且避免"门控丢失"导致提交被静默拒绝。
type commitGate struct {
	mu     sync.RWMutex
	closed map[string]struct{}
}

// newCommitGate 构造一个默认全部开启的门控。
func newCommitGate() *commitGate {
	return &commitGate{closed: make(map[string]struct{})}
}

// open 开启某 Task 的提交门控（Create 时调用）。
func (g *commitGate) open(taskID string) {
	if taskID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.closed, taskID)
}

// close 关闭某 Task 的提交门控。
func (g *commitGate) close(taskID string) {
	if taskID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed[taskID] = struct{}{}
}

// accepting 判断某 Task 是否允许提交；未记录视为允许。
func (g *commitGate) accepting(taskID string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, blocked := g.closed[taskID]
	return !blocked
}

// forget 清理某 Task 的门控记录（Task 被删除时调用）。
func (g *commitGate) forget(taskID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.closed, taskID)
}
