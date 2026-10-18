package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	mockModel "github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// TestStopAndResume 验证用户主动中止与恢复：
// enable → running；stop → stopped + 门控关闭；重新 enable → running + 门控重新打开。
func TestStopAndResume(t *testing.T) {
	ctx := context.Background()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	states := stateMemory.NewStore()
	bus := event.NewBus()
	RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")

	rt := New(runs, tasks, states, mockModel.New(), bus)
	rt.Start()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Shutdown(sctx)
	}()

	if err := tasks.Create(ctx, &domain.Task{ID: "t1", SessionID: "s1", Title: "t", Description: "需求"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// enable → running
	if err := bus.PublishSync(ctx, event.EventEnvelope{Data: event.TaskEnabledEvent{TaskID: "t1"}}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	// stop → stopped + 门控关闭
	if err := bus.PublishSync(ctx, event.EventEnvelope{Data: event.StopRequestedEvent{SessionID: "s1"}}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStatus(t, runs, "t1", domain.RunStatusStopped)

	if err := states.Commit(ctx, "s1", researchstate.CommitInput{ResultContent: "x", FindingContent: "y"}); !errors.Is(err, researchstate.ErrNotAccepting) {
		t.Fatalf("commit after stop should be ErrNotAccepting, got %v", err)
	}

	// 重新 enable → running + 门控重新打开
	if err := bus.PublishSync(ctx, event.EventEnvelope{Data: event.TaskEnabledEvent{TaskID: "t1"}}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	waitStatus(t, runs, "t1", domain.RunStatusRunning)

	if err := states.Commit(ctx, "s1", researchstate.CommitInput{ResultContent: "证据", FindingContent: "结论"}); err != nil {
		t.Fatalf("commit after resume should succeed, got %v", err)
	}
}

func waitStatus(t *testing.T, runs *memory.RunTaskStore, taskID string, want domain.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := runs.GetByTask(context.Background(), taskID)
		if err == nil && rt.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt, _ := runs.GetByTask(context.Background(), taskID)
	t.Fatalf("want status %q, got %+v", want, rt)
}
