package fault

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// captureBus 记录发布的事件。
type captureBus struct {
	mu       sync.Mutex
	events   []event.ErrorOccurredEvent
	failWith error
}

func (b *captureBus) Publish(_ context.Context, e event.EventEnvelope) error {
	if b.failWith != nil {
		return b.failWith
	}
	if ev, ok := e.Data.(event.ErrorOccurredEvent); ok {
		b.mu.Lock()
		b.events = append(b.events, ev)
		b.mu.Unlock()
	}
	return nil
}

func (b *captureBus) snapshot() []event.ErrorOccurredEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]event.ErrorOccurredEvent(nil), b.events...)
}

// fakeClock 提供可推进的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestReporter(window time.Duration) (*Reporter, *captureBus, *fakeClock) {
	bus := &captureBus{}
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := NewReporter(ReporterConfig{Bus: bus, DedupWindow: window, Now: clock.now})
	return r, bus, clock
}

// TestReporterSkipsInfoSeverity 验证 Info 级别只写日志、不推送前端。
func TestReporterSkipsInfoSeverity(t *testing.T) {
	r, bus, _ := newTestReporter(time.Second)

	err := E(CodeInternal, LayerRuntime, "history write failed").
		Severity(SeverityInfo).Build()
	if sent := r.Report(context.Background(), err, Scope{SessionID: "s1"}); sent {
		t.Fatal("info severity should not be pushed to frontend")
	}
	if len(bus.snapshot()) != 0 {
		t.Fatalf("want no events, got %d", len(bus.snapshot()))
	}
}

// TestReporterPushesWarningAndAbove 验证 warning/error/fatal 会推送。
func TestReporterPushesWarningAndAbove(t *testing.T) {
	for _, sev := range []Severity{SeverityWarning, SeverityError, SeverityFatal} {
		r, bus, _ := newTestReporter(time.Second)
		err := E(CodeWorkFailed, LayerWork, "sub-agent failed").Severity(sev).Build()
		if !r.Report(context.Background(), err, Scope{SessionID: "s1", Source: "workpool"}) {
			t.Fatalf("%s should be pushed", sev)
		}
		got := bus.snapshot()
		if len(got) != 1 {
			t.Fatalf("%s: want 1 event, got %d", sev, len(got))
		}
		if got[0].Severity != string(sev) {
			t.Fatalf("%s: severity = %s", sev, got[0].Severity)
		}
	}
}

// TestReporterDedupWindow 验证同类错误在窗口内合并、窗口后可再次推送。
func TestReporterDedupWindow(t *testing.T) {
	r, bus, clock := newTestReporter(5 * time.Second)
	err := E(CodeWorkTimeout, LayerWork, "timeout").Severity(SeverityWarning).
		Source("workpool").Build()
	scope := Scope{SessionID: "s1", TaskID: "t1"}

	// 窗口内连续 3 次：只推送第一次。
	for i := 0; i < 3; i++ {
		r.Report(context.Background(), err, scope)
		clock.advance(500 * time.Millisecond)
	}
	if n := len(bus.snapshot()); n != 1 {
		t.Fatalf("same error within window should merge, got %d events", n)
	}

	// 越过窗口后再次推送，并携带累计次数。
	clock.advance(6 * time.Second)
	r.Report(context.Background(), err, scope)

	got := bus.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 events after window, got %d", len(got))
	}
	if got[1].Count < 2 {
		t.Fatalf("merged event should carry count, got %d", got[1].Count)
	}
}

// TestReporterDifferentCodesNotMerged 验证不同错误码不会互相合并。
func TestReporterDifferentCodesNotMerged(t *testing.T) {
	r, bus, _ := newTestReporter(10 * time.Second)
	scope := Scope{SessionID: "s1", TaskID: "t1"}

	r.Report(context.Background(),
		E(CodeWorkTimeout, LayerWork, "timeout").Severity(SeverityWarning).Build(), scope)
	r.Report(context.Background(),
		E(CodeCommitFailed, LayerResult, "commit").Severity(SeverityError).Build(), scope)

	if n := len(bus.snapshot()); n != 2 {
		t.Fatalf("different codes should both be pushed, got %d", n)
	}
}

// TestReporterWrapsPlainError 验证未包装错误按 error 级别上报。
func TestReporterWrapsPlainError(t *testing.T) {
	r, bus, _ := newTestReporter(time.Second)

	if !r.Report(context.Background(), errors.New("boom"), Scope{SessionID: "s1"}) {
		t.Fatal("plain error should be reported")
	}
	got := bus.snapshot()
	if len(got) != 1 || got[0].Severity != string(SeverityError) {
		t.Fatalf("unexpected event: %+v", got)
	}
}

// TestReporterHidesDetailByDefault 验证默认不下发内部细节。
func TestReporterHidesDetailByDefault(t *testing.T) {
	r, bus, _ := newTestReporter(time.Second)

	err := Wrap(CodeModelUnavailable, LayerInfrastructure, "模型服务不可用",
		errors.New("dial tcp 10.0.0.1:443: refused"))
	r.Report(context.Background(), err, Scope{SessionID: "s1"})

	got := bus.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Detail != "" {
		t.Fatalf("detail should be hidden by default, got %q", got[0].Detail)
	}
	if got[0].Message != "模型服务不可用" {
		t.Fatalf("message = %q", got[0].Message)
	}
}

// TestReporterDebugIncludesDetail 验证 debug 模式下发细节。
func TestReporterDebugIncludesDetail(t *testing.T) {
	bus := &captureBus{}
	r := NewReporter(ReporterConfig{Bus: bus, Debug: true})

	err := Wrap(CodeModelUnavailable, LayerInfrastructure, "模型服务不可用",
		errors.New("dial tcp: refused"))
	r.Report(context.Background(), err, Scope{SessionID: "s1"})

	got := bus.snapshot()
	if len(got) != 1 || got[0].Detail == "" {
		t.Fatalf("debug mode should include detail, got %+v", got)
	}
}

// TestReporterPublishFailureDoesNotPanic 验证事件发布失败时安全降级。
func TestReporterPublishFailureDoesNotPanic(t *testing.T) {
	bus := &captureBus{failWith: errors.New("bus down")}
	r := NewReporter(ReporterConfig{Bus: bus})

	err := E(CodeInternal, LayerRuntime, "x").Severity(SeverityError).Build()
	if r.Report(context.Background(), err, Scope{SessionID: "s1"}) {
		t.Fatal("report should report failure when publish fails")
	}
}

// TestReporterNilBus 验证无总线时只写日志。
func TestReporterNilBus(t *testing.T) {
	r := NewReporter(ReporterConfig{})
	err := E(CodeInternal, LayerRuntime, "x").Severity(SeverityError).Build()
	if r.Report(context.Background(), err, Scope{}) {
		t.Fatal("no bus means no push")
	}
}

// TestReporterNilError 验证 nil 错误不上报。
func TestReporterNilError(t *testing.T) {
	r, bus, _ := newTestReporter(time.Second)
	if r.Report(context.Background(), nil, Scope{}) {
		t.Fatal("nil error should not be reported")
	}
	if len(bus.snapshot()) != 0 {
		t.Fatal("nil error should not publish events")
	}
}
