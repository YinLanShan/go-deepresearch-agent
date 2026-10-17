package runtime

import (
	"context"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// MockCoordinator 模拟一个依赖组件（如 Supervisor、Scheduler、WorkPool）。
//
// 它订阅 Runtime 发起的 CoordinationRequested 协商事件，并回执"同意"或"拒绝"。
// 本阶段用它占位；后续把 mockLifecycleEnd 替换为真实依赖组件的协作式终止逻辑。
type MockCoordinator struct {
	name      string
	rejectErr error // 非空则协商时返回该错误（模拟拒绝）
}

// NewMockCoordinator 构造一个默认"同意"的模拟依赖组件。
func NewMockCoordinator(name string) *MockCoordinator {
	return &MockCoordinator{name: name}
}

// WithReject 配置该组件在协商时返回指定错误（用于测试拒绝路径）。
func (m *MockCoordinator) WithReject(err error) *MockCoordinator {
	m.rejectErr = err
	return m
}

// 编译期断言实现两个投递接口。
var (
	_ event.EventSender     = (*MockCoordinator)(nil)
	_ event.SyncEventSender = (*MockCoordinator)(nil)
)

// Send 异步协商（本场景不使用，仅满足 EventSender 接口）。
func (m *MockCoordinator) Send(_ context.Context, _ event.EventEnvelope) error {
	return m.result()
}

// SendSync 同步协商：回执"同意"或"拒绝"。
func (m *MockCoordinator) SendSync(_ context.Context, _ event.EventEnvelope, h *event.Handshake) error {
	h.Reply(m.result())
	return nil
}

// result 返回协商结果：默认同意，配置 WithReject 后拒绝。
func (m *MockCoordinator) result() error {
	if m.rejectErr != nil {
		return m.rejectErr
	}
	// 模拟协作式结束生命周期（占位，后续替换为真实 Supervisor/Scheduler/WorkPool 的终止逻辑）
	return nil
}

// RegisterMockCoordinators 注册一组模拟依赖组件并返回它们，便于测试时配置。
func RegisterMockCoordinators(bus *event.Bus, names ...string) []*MockCoordinator {
	coords := make([]*MockCoordinator, 0, len(names))
	for _, n := range names {
		c := NewMockCoordinator(n)
		bus.Subscribe(event.EventCoordinationRequested, c)
		coords = append(coords, c)
	}
	return coords
}
