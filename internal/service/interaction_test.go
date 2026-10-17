package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// newInteraction 装配一条完整的事件驱动链路：
//
//	RunService(发布) -> Bus -> Runtime(订阅，协商后原子落地) + 模拟依赖组件。
func newInteraction() (*RunService, *memory.TaskStore, *memory.RunTaskStore, *runtime.Runtime) {
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	bus := event.NewBus()
	runtime.RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")
	rt := runtime.New(runs, bus)
	rt.Start()
	return NewRunService(tasks, runs, bus), tasks, runs, rt
}

func shutdownRuntime(t *testing.T, rt *runtime.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rt.Shutdown(ctx); err != nil {
		t.Errorf("runtime shutdown: %v", err)
	}
}

func getRunTaskStatus(t *testing.T, runs storage.RunTaskStore, taskID string) domain.RunStatus {
	t.Helper()
	rt, err := runs.GetByTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	return rt.Status
}

// TestTaskEnableDisableInteraction 验证协商式的启用/禁用：
// 启用 -> Runtime 协商后置 running；禁用 -> 协商后 paused；再次启用 -> running。
// 由于 SetTaskEnabled 使用同步投递，返回时状态已落地，故直接断言。
func TestTaskEnableDisableInteraction(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rt := newInteraction()
	defer shutdownRuntime(t, rt)

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Enabled: false}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 前置：RunTask 已存在（enable 不负责创建）
	if err := runs.Create(ctx, &domain.RunTask{ID: "r1", TaskID: "t1", Status: domain.RunStatusCreated}); err != nil {
		t.Fatalf("create run task: %v", err)
	}

	// 1) 使能
	if _, err := svc.SetTaskEnabled(ctx, "t1", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := getRunTaskStatus(t, runs, "t1"); got != domain.RunStatusRunning {
		t.Fatalf("want running, got %s", got)
	}

	// 2) 失能
	if _, err := svc.SetTaskEnabled(ctx, "t1", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := getRunTaskStatus(t, runs, "t1"); got != domain.RunStatusPaused {
		t.Fatalf("want paused, got %s", got)
	}

	// 3) 再次使能
	if _, err := svc.SetTaskEnabled(ctx, "t1", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if got := getRunTaskStatus(t, runs, "t1"); got != domain.RunStatusRunning {
		t.Fatalf("want running, got %s", got)
	}
}

// TestEnableCreatesRunTaskWhenMissing 使能时若无 RunTask，Runtime 协商创建后新建。
func TestEnableCreatesRunTaskWhenMissing(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rt := newInteraction()
	defer shutdownRuntime(t, rt)

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Enabled: false}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 未创建 RunTask：使能应协商创建并新建 RunTask(running)
	if _, err := svc.SetTaskEnabled(ctx, "t1", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := getRunTaskStatus(t, runs, "t1"); got != domain.RunStatusRunning {
		t.Fatalf("want running, got %s", got)
	}
}

// TestRequestDeleteRunTaskNegotiated 删除请求经 Runtime 协商后原子删除。
func TestRequestDeleteRunTaskNegotiated(t *testing.T) {
	ctx := context.Background()
	svc, tasks, runs, rt := newInteraction()
	defer shutdownRuntime(t, rt)

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Enabled: false}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := runs.Create(ctx, &domain.RunTask{ID: "r1", TaskID: "t1", Status: domain.RunStatusRunning}); err != nil {
		t.Fatalf("create run task: %v", err)
	}

	if err := svc.RequestDeleteRunTask(ctx, "t1"); err != nil {
		t.Fatalf("RequestDeleteRunTask: %v", err)
	}
	if _, err := runs.GetByTask(ctx, "t1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want run task deleted, got %v", err)
	}
}

// TestRequestDeleteRunTaskRejected 依赖组件拒绝协商时，删除失败且 RunTask 不被删除。
func TestRequestDeleteRunTaskRejected(t *testing.T) {
	ctx := context.Background()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	bus := event.NewBus()
	coords := runtime.RegisterMockCoordinators(bus, "supervisor")
	coords[0].WithReject(errors.New("supervisor: not ready to terminate"))
	rt := runtime.New(runs, bus)
	rt.Start()
	defer shutdownRuntime(t, rt)
	svc := NewRunService(tasks, runs, bus)

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Enabled: false}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := runs.Create(ctx, &domain.RunTask{ID: "r1", TaskID: "t1", Status: domain.RunStatusRunning}); err != nil {
		t.Fatalf("create run task: %v", err)
	}

	if err := svc.RequestDeleteRunTask(ctx, "t1"); err == nil {
		t.Fatal("want error when coordinator rejects")
	}
	if _, err := runs.GetByTask(ctx, "t1"); err != nil {
		t.Fatalf("run task should still exist after rejected delete, got %v", err)
	}
}
