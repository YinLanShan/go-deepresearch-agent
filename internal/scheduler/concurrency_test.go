package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// TestMaxConcurrencyLimitsInFlightWork 验证 MaxConcurrency 限制的是
// "同时在途"的 Work（排队 + 执行），而不是并行执行数。
//
// 场景：MaxConcurrency=1，一次登记 2 个 Assignment。
// Scheduler 的容量在 Work 进入终态后才归还，因此第 2 个 Assignment
// 会停在队列里，直到第 1 个 Work 结束。
func TestMaxConcurrencyLimitsInFlightWork(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, _, bus := newTestScheduler(t, pool, slots, 1)
	defer func() { _ = s.Shutdown(context.Background()) }()

	ctx := context.Background()
	a1 := newAssignment("a1", "r1")
	a2 := newAssignment("a2", "r1")

	// 注意：Assignment 由 Scheduler 在 enqueue 时创建，测试不要预先 Create，
	// 否则会因主键冲突被拒绝、根本不会进入调度队列。
	//
	// 一次性提交两个 Assignment。
	if err := bus.PublishSync(ctx, event.EventEnvelope{
		Data: event.AssignmentSubmittedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			Assignments: []*domain.Assignment{a1, a2},
		},
	}); err != nil {
		t.Fatalf("publish assignments: %v", err)
	}

	// 容量为 1：只应有 1 个 Work 被提交。
	if !waitUntil(2*time.Second, func() bool {
		return len(pool.submittedWorks()) == 1
	}) {
		t.Fatalf("expected exactly 1 in-flight work, got %d", len(pool.submittedWorks()))
	}

	// 再等一下，确认第 2 个不会越过容量限制。
	time.Sleep(150 * time.Millisecond)
	if n := len(pool.submittedWorks()); n != 1 {
		t.Fatalf("max concurrency should cap in-flight works at 1, got %d", n)
	}

	// 让第一个 Work 结束，容量归还后第 2 个才会被提交。
	w1 := pool.submittedWorks()[0]
	if err := bus.Publish(ctx, event.EventEnvelope{
		Data: event.WorkUpdatedEvent{
			SessionID: "s1", TaskID: "t1", RunTaskID: "r1",
			AssignmentID: w1.AssignmentID, WorkID: w1.ID,
			Status: domain.WorkSucceeded,
		},
	}); err != nil {
		t.Fatalf("publish work updated: %v", err)
	}

	if !waitUntil(2*time.Second, func() bool {
		return len(pool.submittedWorks()) == 2
	}) {
		t.Fatalf("second work should start after the first completes, got %d",
			len(pool.submittedWorks()))
	}
}

// TestMaxConcurrencyDefaultsToTwo 验证未配置时的默认值。
func TestMaxConcurrencyDefaultsToTwo(t *testing.T) {
	pool := &fakePool{}
	slots := &fakeSlots{}
	s, _, _ := newTestScheduler(t, pool, slots, 0) // 0 → 默认值
	defer func() { _ = s.Shutdown(context.Background()) }()

	if s.maxConc != 2 {
		t.Fatalf("default max concurrency should be 2, got %d", s.maxConc)
	}
}

// waitUntil 轮询条件直到超时。
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
