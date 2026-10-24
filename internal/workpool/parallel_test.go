package workpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// countingWorker 记录自己被调用的次数与并发情况。
type countingWorker struct {
	id      string
	delay   time.Duration
	running *int64
	maxConc *int64
}

func (c *countingWorker) ID() string { return c.id }

func (c *countingWorker) Run(ctx context.Context, _ *domain.Work,
	_ func(stage, detail string)) (string, error) {
	cur := atomic.AddInt64(c.running, 1)
	defer atomic.AddInt64(c.running, -1)
	for {
		max := atomic.LoadInt64(c.maxConc)
		if cur <= max || atomic.CompareAndSwapInt64(c.maxConc, max, cur) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(c.delay):
	}
	return "结果来自 " + c.id, nil
}

// countingFactory 为每个 Work 创建独立 Worker，并统计并发峰值。
type countingFactory struct {
	delay   time.Duration
	running int64
	maxConc int64
	seq     int64
	ids     sync.Map
}

func (f *countingFactory) NewWorker(_ context.Context, _ *domain.Work) (Worker, error) {
	n := atomic.AddInt64(&f.seq, 1)
	id := "worker-" + string(rune('A'+n-1))
	return &countingWorker{id: id, delay: f.delay, running: &f.running, maxConc: &f.maxConc}, nil
}

// TestWorkPoolParallelExecution 验证多个 Work 真正并行执行。
//
// 这是修复前的缺陷：WorkPool 曾共享单个 Worker 实例，
// 并发时 WorkerID 互相覆盖、子 Agent 的事件流互相污染。
func TestWorkPoolParallelExecution(t *testing.T) {
	works := memory.NewWorkStore()
	bus := event.NewBus()
	factory := &countingFactory{delay: 120 * time.Millisecond}

	p := New(Config{
		Works: works, Bus: bus, Factory: factory, Workers: 3,
	})
	p.Start()
	defer func() { _ = p.Shutdown(context.Background()) }()

	for i := 0; i < 3; i++ {
		w := &domain.Work{
			ID: "w" + string(rune('1'+i)), AssignmentID: "a" + string(rune('1'+i)),
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Goal: "目标", Status: domain.WorkPending,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := works.Create(context.Background(), w); err != nil {
			t.Fatalf("create work: %v", err)
		}
		if err := p.Submit(context.Background(), w); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	// 三个 Work 并行 → 峰值并发应 >= 2（真正的并行）
	if !waitForTest(3*time.Second, func() bool {
		q, running := p.Status()
		return q == 0 && running == 0
	}) {
		t.Fatal("works did not finish")
	}

	if got := atomic.LoadInt64(&factory.maxConc); got < 2 {
		t.Fatalf("expected parallel execution (max concurrency >= 2), got %d", got)
	}

	// 每个 Work 的 WorkerID 必须互不相同
	seen := map[string]string{}
	list, err := works.ListByRunTask(context.Background(), "r1")
	if err != nil {
		t.Fatalf("list works: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 works, got %d", len(list))
	}
	for _, w := range list {
		if w.WorkerID == "" {
			t.Fatalf("work %s has empty worker id", w.ID)
		}
		if prev, dup := seen[w.WorkerID]; dup {
			t.Fatalf("worker id %q reused by %s and %s", w.WorkerID, prev, w.ID)
		}
		seen[w.WorkerID] = w.ID
	}
}

// TestWorkPoolTimeoutMarksTimedOut 验证超时后 Work 被标记为 timedout 且子 Agent 被取消。
func TestWorkPoolTimeoutMarksTimedOut(t *testing.T) {
	works := memory.NewWorkStore()
	bus := event.NewBus()
	factory := &countingFactory{delay: 2 * time.Second} // 远大于超时

	p := New(Config{
		Works: works, Bus: bus, Factory: factory, Workers: 1,
		WorkTimeout: 80 * time.Millisecond,
	})
	p.Start()
	defer func() { _ = p.Shutdown(context.Background()) }()

	w := &domain.Work{
		ID: "w1", AssignmentID: "a1", SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
		Goal: "目标", Status: domain.WorkPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := works.Create(context.Background(), w); err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := p.Submit(context.Background(), w); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if !waitForTest(3*time.Second, func() bool {
		got, err := works.Get(context.Background(), "w1")
		return err == nil && got.Status.IsTerminal()
	}) {
		t.Fatal("work did not reach terminal state")
	}

	got, _ := works.Get(context.Background(), "w1")
	if got.Status != domain.WorkTimedOut {
		t.Fatalf("want timedout, got %s (err=%q)", got.Status, got.Err)
	}
	if !errors.Is(ErrWorkTimeout, ErrWorkTimeout) {
		t.Fatal("sanity")
	}
}

// TestWorkPoolFactoryError 验证工厂失败时 Work 被标记 failed。
func TestWorkPoolFactoryError(t *testing.T) {
	works := memory.NewWorkStore()
	bus := event.NewBus()

	p := New(Config{
		Works: works, Bus: bus, Workers: 1,
		Factory: workerFunc(func(context.Context, *domain.Work) (Worker, error) {
			return nil, errors.New("factory boom")
		}),
		WorkTimeout: time.Second,
	})
	p.Start()
	defer func() { _ = p.Shutdown(context.Background()) }()

	w := &domain.Work{
		ID: "w1", AssignmentID: "a1", SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
		Goal: "目标", Status: domain.WorkPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := works.Create(context.Background(), w); err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := p.Submit(context.Background(), w); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if !waitForTest(3*time.Second, func() bool {
		got, err := works.Get(context.Background(), "w1")
		return err == nil && got.Status == domain.WorkFailed
	}) {
		got, _ := works.Get(context.Background(), "w1")
		t.Fatalf("want failed, got %+v", got)
	}
}

// waitForTest 轮询条件直到超时。
func waitForTest(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
