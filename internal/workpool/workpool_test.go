package workpool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// collectEvents 收集某类事件。
type collectEvents struct {
	mu      sync.Mutex
	works   []event.WorkUpdatedEvent
	results []event.WorkResultReadyEvent
}

var _ event.EventSender = (*collectEvents)(nil)

func (c *collectEvents) Send(_ context.Context, e event.EventEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev := e.Data.(type) {
	case event.WorkUpdatedEvent:
		c.works = append(c.works, ev)
	case event.WorkResultReadyEvent:
		c.results = append(c.results, ev)
	}
	return nil
}

func (c *collectEvents) workStages(workID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.works))
	for _, w := range c.works {
		if w.WorkID == workID {
			out = append(out, w.Stage)
		}
	}
	return out
}

func (c *collectEvents) resultCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.results)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func newTestPool(t *testing.T, workers int, delay time.Duration) (*WorkPool, *memory.WorkStore, *collectEvents) {
	t.Helper()
	works := memory.NewWorkStore()
	bus := event.NewBus()
	rec := &collectEvents{}
	bus.Subscribe(event.EventWorkUpdated, rec)
	bus.Subscribe(event.EventWorkResultReady, rec)

	p := New(Config{
		Works:   works,
		Bus:     bus,
		Worker:  NewMockWorker(delay),
		Workers: workers,
	})
	p.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p, works, rec
}

func newWork(id, runTaskID string) *domain.Work {
	now := time.Now()
	return &domain.Work{
		ID: id, AssignmentID: "as-" + id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.WorkPending, Attempt: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}

// TestWorkPoolRunsMockWorker 验证 MockWorker 会推进阶段并产出结果。
func TestWorkPoolRunsMockWorker(t *testing.T) {
	p, works, rec := newTestPool(t, 2, 5*time.Millisecond)

	w := newWork("w1", "r1")
	if err := works.Create(context.Background(), w); err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := p.Submit(context.Background(), w); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		got, err := works.Get(context.Background(), "w1")
		return err == nil && got.Status == domain.WorkSucceeded
	}) {
		got, _ := works.Get(context.Background(), "w1")
		t.Fatalf("work not succeeded: %+v", got)
	}

	got, _ := works.Get(context.Background(), "w1")
	if got.Result == "" {
		t.Fatal("work result should not be empty")
	}
	if got.WorkerID == "" {
		t.Fatal("work should record worker id")
	}

	// 阶段必须按序上报（前端据此展示子 Agent 实时进度）
	stages := rec.workStages("w1")
	wantOrder := []string{"starting", "planning", "retrieving", "analyzing", "summarizing", "finished"}
	if len(stages) < len(wantOrder) {
		t.Fatalf("stages = %v, want at least %v", stages, wantOrder)
	}
	for i, want := range wantOrder {
		if stages[i] != want {
			t.Fatalf("stage[%d] = %q, want %q (all=%v)", i, stages[i], want, stages)
		}
	}

	// 成功后必须广播 WorkResultReady，交给 ResultAggregator 提交
	if !waitFor(t, 2*time.Second, func() bool { return rec.resultCount() == 1 }) {
		t.Fatal("work result ready event not published")
	}
}

// TestWorkPoolCancelsOnStop 验证全局中止只做状态标记（不等待真实中断）。
func TestWorkPoolCancelsOnStop(t *testing.T) {
	p, works, _ := newTestPool(t, 1, 300*time.Millisecond)

	w := newWork("w1", "r1")
	if err := works.Create(context.Background(), w); err != nil {
		t.Fatalf("create work: %v", err)
	}
	// 占住唯一 Worker，使第二个 Work 只能排队。
	blocker := newWork("w0", "r1")
	if err := works.Create(context.Background(), blocker); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := p.Submit(context.Background(), blocker); err != nil {
		t.Fatalf("submit blocker: %v", err)
	}
	if err := p.Submit(context.Background(), w); err != nil {
		t.Fatalf("submit: %v", err)
	}

	n := p.Cancel("r1")
	if n == 0 {
		t.Fatal("cancel should affect at least one work")
	}

	if !waitFor(t, 3*time.Second, func() bool {
		got, err := works.Get(context.Background(), "w1")
		return err == nil && got.Status == domain.WorkCancelled
	}) {
		got, _ := works.Get(context.Background(), "w1")
		t.Fatalf("queued work should be cancelled, got %+v", got)
	}
}

// TestWorkPoolConcurrency 验证并发受 Worker 数量限制。
func TestWorkPoolConcurrency(t *testing.T) {
	p, works, _ := newTestPool(t, 2, 5*time.Millisecond)

	for i := 0; i < 6; i++ {
		w := newWork("w"+string(rune('1'+i)), "r1")
		if err := works.Create(context.Background(), w); err != nil {
			t.Fatalf("create work: %v", err)
		}
		if err := p.Submit(context.Background(), w); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	if !waitFor(t, 10*time.Second, func() bool {
		list, err := works.ListByRunTask(context.Background(), "r1")
		if err != nil {
			return false
		}
		for _, w := range list {
			if w.Status != domain.WorkSucceeded {
				return false
			}
		}
		return len(list) == 6
	}) {
		list, _ := works.ListByRunTask(context.Background(), "r1")
		t.Fatalf("all works should succeed, got %d", len(list))
	}
}
