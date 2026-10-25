package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// TestSessionEventsRequiresExistingSession 验证会话级 SSE 端点复用统一的前置校验。
func TestSessionEventsRequiresExistingSession(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "GET", "/api/v1/sessions/no-such/events", nil)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404 for unknown session, got %d", resp.StatusCode())
	}
}

// TestRunStatusEventPublishedOnEnable 验证启用 Task 会发布 running 状态事件。
func TestRunStatusEventPublishedOnEnable(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	sessionID, taskID := createSessionAndTask(t, router)
	sub := &runStatusRecorder{}
	h.api.runs.Bus().Subscribe(event.EventRunStatusChanged, sub)
	defer h.api.runs.Bus().Unsubscribe(event.EventRunStatusChanged, sub)

	w := ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": true}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("enable: want 200, got %d", resp.StatusCode())
	}

	if !waitForCond(5*time.Second, func() bool { return sub.has(domain.RunStatusRunning) }) {
		t.Fatalf("expected running status event, got %v", sub.snapshot())
	}
	// 事件必须带正确的 Session，供 SSE 按会话过滤。
	if !sub.hasSession(sessionID) {
		t.Fatalf("event should carry session %s, got %v", sessionID, sub.sessionSnapshot())
	}
}

// TestRunStatusEventOnFinish 验证研究结束时发布 finished 事件。
func TestRunStatusEventOnFinish(t *testing.T) {
	h := newHarnessWith("mock-tool")
	defer h.stop(t)
	router := h.router(t)

	sessionID, taskID := createSessionAndTask(t, router)
	sub := &runStatusRecorder{}
	h.api.runs.Bus().Subscribe(event.EventRunStatusChanged, sub)
	defer h.api.runs.Bus().Unsubscribe(event.EventRunStatusChanged, sub)

	w := ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": true}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("enable: want 200, got %d", resp.StatusCode())
	}
	waitRunStatus(t, h.runs, taskID, domain.RunStatusRunning)

	// 投递一条消息，mock-tool 会调用 finish_research。
	waitForCond(5*time.Second, func() bool { return true })
	if err := h.api.runs.SendUserMessage(context.Background(), sessionID, "请分析"); err != nil {
		t.Fatalf("send message: %v", err)
	}

	if !waitForCond(10*time.Second, func() bool { return sub.has(domain.RunStatusFinished) }) {
		t.Fatalf("expected finished status event, got %v", sub.snapshot())
	}
}

// TestBusUnsubscribe 验证取消订阅后不再收到事件。
//
// 长连接关闭时必须解除订阅，否则每次重连都会留下失效订阅者。
func TestBusUnsubscribe(t *testing.T) {
	bus := event.NewBus()
	rec := &runStatusRecorder{}
	bus.Subscribe(event.EventRunStatusChanged, rec)
	bus.Unsubscribe(event.EventRunStatusChanged, rec)

	if err := bus.Publish(context.Background(), event.EventEnvelope{
		Data: event.RunStatusChangedEvent{
			SessionID: "s1", TaskID: "t1", Status: domain.RunStatusRunning,
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("unsubscribed recorder should receive nothing, got %v", rec.snapshot())
	}
}

// TestBusUnsubscribeIsIdempotent 验证重复取消订阅不会 panic。
func TestBusUnsubscribeIsIdempotent(t *testing.T) {
	bus := event.NewBus()
	rec := &runStatusRecorder{}
	bus.Subscribe(event.EventRunStatusChanged, rec)
	bus.Unsubscribe(event.EventRunStatusChanged, rec)
	bus.Unsubscribe(event.EventRunStatusChanged, rec) // 不应 panic
}

// runStatusRecorder 记录收到的 RunTask 状态事件。
type runStatusRecorder struct {
	mu       sync.Mutex
	statuses []domain.RunStatus
	sessions []string
}

func (r *runStatusRecorder) Send(_ context.Context, e event.EventEnvelope) error {
	ev, ok := e.Data.(event.RunStatusChangedEvent)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, ev.Status)
	r.sessions = append(r.sessions, ev.SessionID)
	return nil
}

func (r *runStatusRecorder) has(s domain.RunStatus) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.statuses {
		if got == s {
			return true
		}
	}
	return false
}

func (r *runStatusRecorder) hasSession(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.sessions {
		if got == id {
			return true
		}
	}
	return false
}

func (r *runStatusRecorder) snapshot() []domain.RunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.RunStatus(nil), r.statuses...)
}

func (r *runStatusRecorder) sessionSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sessions...)
}

// waitForCond 轮询条件直到超时。
func waitForCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
