package resultagg

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
)

// fakeScheduler 记录提交确认与拒绝。
type fakeScheduler struct {
	mu        sync.Mutex
	committed []string
	rejected  []string
}

func (f *fakeScheduler) OnResultCommitted(assignmentID, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, assignmentID)
}

func (f *fakeScheduler) OnResultRejected(assignmentID string, _ error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, assignmentID)
}

func (f *fakeScheduler) snapshot() (committed, rejected []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.committed...), append([]string(nil), f.rejected...)
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

// newTestAgg 构造聚合器与它依赖的 ResearchState 存储。
func newTestAgg(t *testing.T) (*Aggregator, *stateMemory.Store, *fakeScheduler, *event.Bus) {
	t.Helper()
	states := stateMemory.NewStore()
	if err := states.Create(context.Background(), researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}
	bus := event.NewBus()
	sched := &fakeScheduler{}
	a := New(Config{States: states, Bus: bus, Scheduler: sched})
	a.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a, states, sched, bus
}

func slotAssignment(id, runTaskID string) *domain.Assignment {
	return &domain.Assignment{
		ID: id, SessionID: "s1", TaskID: "t1", RunTaskID: runTaskID,
		Goal: "目标 " + id, Status: domain.AssignmentScheduled,
	}
}

// TestAggregatorBatchCommit 验证同一 Task 的多个 Result 都能被提交（可合并为一批）。
//
// 注意：批处理边界是"当前已到达且已登记的结果"，两个结果若被分别唤醒处理，
// 就会落成两次 Commit。因此这里断言"两条证据都进入历史、且每个 Assignment
// 都收到提交确认"，而不断言批次数量——那取决于调度时序，不是契约。
func TestAggregatorBatchCommit(t *testing.T) {
	a, states, sched, bus := newTestAgg(t)

	for _, id := range []string{"a1", "a2"} {
		if err := a.Reserve(slotAssignment(id, "r1")); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}

	for _, item := range []struct{ assignment, work, result string }{
		{"a1", "w1", "证据一"},
		{"a2", "w2", "证据二"},
	} {
		if err := bus.Publish(context.Background(), event.EventEnvelope{
			Data: event.WorkResultReadyEvent{
				SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
				AssignmentID: item.assignment, WorkID: item.work, Result: item.result,
			},
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	if !waitFor(t, 3*time.Second, func() bool {
		committed, _ := sched.snapshot()
		return len(committed) == 2
	}) {
		committed, rejected := sched.snapshot()
		t.Fatalf("commit confirmation missing, committed=%v rejected=%v", committed, rejected)
	}

	st, err := states.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Version == 0 {
		t.Fatal("research state should be committed")
	}

	// 两条证据都必须进入历史（合并或分次提交都可以，但不能丢失）
	var all strings.Builder
	for _, r := range st.Results {
		all.WriteString(r.Content)
		all.WriteString("\n")
	}
	for _, want := range []string{"证据一", "证据二"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("committed results missing %q, got %q", want, all.String())
		}
	}
}

// TestAggregatorRejectsWithoutSlot 验证未预留槽位的结果不会被提交。
func TestAggregatorRejectsWithoutSlot(t *testing.T) {
	_, states, sched, bus := newTestAgg(t)

	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkResultReadyEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: "unknown", WorkID: "w1", Result: "证据",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	committed, _ := sched.snapshot()
	if len(committed) != 0 {
		t.Fatalf("unknown slot must not be committed, got %v", committed)
	}
	st, _ := states.Get(context.Background(), "t1")
	if st.Version != 0 {
		t.Fatal("research state should not change for unknown slot")
	}
}

// TestAggregatorCommitFailureKeepsSlot 验证 Commit 失败时保留槽位以便重试。
func TestAggregatorCommitFailureKeepsSlot(t *testing.T) {
	states := stateMemory.NewStore()
	if err := states.Create(context.Background(), researchstate.New("id1", "t1", "s1", 1)); err != nil {
		t.Fatalf("create state: %v", err)
	}
	// 关闭提交门控，制造 Commit 失败
	if err := states.StopCommits(context.Background(), "t1"); err != nil {
		t.Fatalf("stop commits: %v", err)
	}

	bus := event.NewBus()
	sched := &fakeScheduler{}
	a := New(Config{States: states, Bus: bus, Scheduler: sched})
	a.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()

	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkResultReadyEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: "a1", WorkID: "w1", Result: "证据",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		_, rejected := sched.snapshot()
		return len(rejected) == 1
	}) {
		t.Fatal("commit failure should reject the assignment")
	}
	// 槽位应保留（允许后续重试），因此仍在 slots 中
	if _, slots := a.Status(); slots != 1 {
		t.Fatalf("slot should be kept for retry, got %d", slots)
	}
}

// TestAggregatorDropsOnStop 验证全局中止会丢弃待提交结果。
func TestAggregatorDropsOnStop(t *testing.T) {
	a, states, sched, bus := newTestAgg(t)

	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := bus.PublishSync(context.Background(), event.EventEnvelope{
		Data: event.CoordinationRequestedEvent{TaskID: "t1", RunTaskID: "r1", Action: event.ActionStop},
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// 中止后到达的结果应被丢弃（槽位已删）
	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkResultReadyEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: "a1", WorkID: "w1", Result: "证据",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	committed, _ := sched.snapshot()
	if len(committed) != 0 {
		t.Fatalf("result after stop must not be committed, got %v", committed)
	}
	st, _ := states.Get(context.Background(), "t1")
	if st.Version != 0 {
		t.Fatal("research state should not change after stop")
	}
}

// TestAggregatorReserveDuplicate 验证重复预留同一 Assignment 会报错。
func TestAggregatorReserveDuplicate(t *testing.T) {
	a, _, _, _ := newTestAgg(t)
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := a.Reserve(slotAssignment("a1", "r1")); err == nil {
		t.Fatal("duplicate reserve should fail")
	}
}

// TestAggregatorEmptyResultRejected 验证空结果不会写入 ResearchState。
func TestAggregatorEmptyResultRejected(t *testing.T) {
	a, states, sched, bus := newTestAgg(t)
	if err := a.Reserve(slotAssignment("a1", "r1")); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.WorkResultReadyEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: "a1", WorkID: "w1", Result: "   ",
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		_, rejected := sched.snapshot()
		return len(rejected) == 1
	}) {
		t.Fatal("empty result should be rejected")
	}
	st, _ := states.Get(context.Background(), "t1")
	if st.Version != 0 {
		t.Fatal("empty result must not be committed")
	}
}
