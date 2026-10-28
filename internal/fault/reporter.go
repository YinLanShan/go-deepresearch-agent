package fault

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
)

// EventPublisher 是 Reporter 对事件总线的最小依赖。
type EventPublisher interface {
	Publish(ctx context.Context, e event.EventEnvelope) error
}

// ReporterConfig 是错误上报器的配置。
type ReporterConfig struct {
	// Bus 用于发布 ErrorOccurred 事件；为 nil 时只写日志。
	Bus EventPublisher
	// DedupWindow 是同类错误的合并窗口；<=0 时使用 DefaultDedupWindow。
	DedupWindow time.Duration
	// Debug 为 true 时把底层 Detail 一并下发（仅开发环境）。
	Debug bool
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// DefaultDedupWindow 是默认的同类错误合并窗口。
//
// 高频失败（如连续 10 个子任务超时）若逐条推送会刷屏，
// 因此同一 (session, task, code, source) 在窗口内只上报一次并累计次数。
const DefaultDedupWindow = 5 * time.Second

// Reporter 把错误按"是否值得上报"分流：写日志 + 必要时发布 ErrorOccurred 事件。
//
// 设计要点：
//   - 只负责"上报"，不改变业务状态（状态变更仍由责任组件完成）；
//   - 按 Severity 决定是否推送前端：Info 只写日志；
//   - 同类错误在时间窗口内合并，用 Count 表达发生次数。
type Reporter struct {
	bus         EventPublisher
	dedupWindow time.Duration
	debug       bool
	now         func() time.Time

	mu   sync.Mutex
	seen map[string]*dedupEntry // key -> 最近一次上报信息
}

// dedupEntry 记录某个 key 在窗口内的累计情况。
type dedupEntry struct {
	count      int
	firstAt    time.Time
	lastSentAt time.Time
}

// NewReporter 构造错误上报器。
func NewReporter(cfg ReporterConfig) *Reporter {
	window := cfg.DedupWindow
	if window <= 0 {
		window = DefaultDedupWindow
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Reporter{
		bus:         cfg.Bus,
		dedupWindow: window,
		debug:       cfg.Debug,
		now:         now,
		seen:        make(map[string]*dedupEntry),
	}
}

// Report 上报一个错误。
//
// 无论是否推送前端，都会写日志（错误不应静默消失）。
// 返回是否实际向前端推送了事件。
func (r *Reporter) Report(ctx context.Context, err error, scope Scope) bool {
	if err == nil {
		return false
	}

	var fe *Error
	if !errors.As(err, &fe) {
		// 未包装错误：按 error 级别上报，保持可观测。
		fe = E(CodeInternal, LayerRuntime, "unclassified error").
			Severity(SeverityError).
			Source(scope.Source).
			Wrap(err)
	}

	severity := fe.Severity
	if severity == "" {
		severity = SeverityError
	}

	logError(fe, scope, severity)

	// Info 级别只写日志，不打扰前端。
	if severity == SeverityInfo {
		return false
	}
	if r.bus == nil {
		return false
	}

	key := dedupKey(fe, scope)
	count, shouldSend := r.allow(key)
	if !shouldSend {
		return false
	}

	detail := ""
	if r.debug && fe.Err != nil {
		detail = fe.Err.Error()
	}
	ev := event.ErrorOccurredEvent{
		SessionID:    scope.SessionID,
		TaskID:       firstNonEmpty(scope.TaskID, fe.TaskID),
		RunTaskID:    firstNonEmpty(scope.RunTaskID, fe.RunTaskID),
		Code:         string(fe.Code),
		Layer:        string(fe.Layer),
		Severity:     string(severity),
		Source:       firstNonEmpty(fe.Source, scope.Source),
		Message:      fe.ClientMessage(),
		Detail:       detail,
		AssignmentID: firstNonEmpty(scope.AssignmentID, fe.AssignmentID),
		WorkID:       firstNonEmpty(scope.WorkID, fe.WorkID),
		Count:        count,
		OccurredAt:   fe.OccurredAt,
	}
	if err := r.bus.Publish(ctx, event.EventEnvelope{Data: ev}); err != nil {
		slog.Warn("fault: publish error event failed",
			"code", string(fe.Code), "err", err)
		return false
	}
	return true
}

// Scope 补充错误的业务归属（fault.Error 未携带时使用）。
type Scope struct {
	SessionID    string
	TaskID       string
	RunTaskID    string
	AssignmentID string
	WorkID       string
	// Source 是调用方组件名（当 fault.Error 未设置 Source 时使用）。
	Source string
}

// allow 判断某个 key 是否允许上报，并返回窗口内累计次数。
func (r *Reporter) allow(key string) (count int, send bool) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	// 顺带清理过期条目，避免 map 无限增长。
	for k, e := range r.seen {
		if now.Sub(e.lastSentAt) > 2*r.dedupWindow {
			delete(r.seen, k)
		}
	}

	entry, ok := r.seen[key]
	if !ok {
		r.seen[key] = &dedupEntry{count: 1, firstAt: now, lastSentAt: now}
		return 1, true
	}
	entry.count++
	if now.Sub(entry.lastSentAt) < r.dedupWindow {
		return entry.count, false // 窗口内：只累计，不重复推送
	}
	entry.lastSentAt = now
	return entry.count, true
}

// dedupKey 构造合并键：同会话、同任务、同错误码、同来源视为同类。
func dedupKey(fe *Error, scope Scope) string {
	return firstNonEmpty(scope.SessionID, "") + "|" +
		firstNonEmpty(scope.TaskID, fe.TaskID) + "|" +
		string(fe.Code) + "|" +
		firstNonEmpty(fe.Source, scope.Source)
}

// logError 按严重级别写日志。
func logError(fe *Error, scope Scope, severity Severity) {
	args := []any{
		"code", string(fe.Code),
		"layer", string(fe.Layer),
		"severity", string(severity),
		"source", firstNonEmpty(fe.Source, scope.Source),
		"task", firstNonEmpty(scope.TaskID, fe.TaskID),
		"run_task", firstNonEmpty(scope.RunTaskID, fe.RunTaskID),
		"assignment", firstNonEmpty(scope.AssignmentID, fe.AssignmentID),
		"work", firstNonEmpty(scope.WorkID, fe.WorkID),
		"msg", fe.Msg,
	}
	if fe.Err != nil {
		args = append(args, "err", fe.Err)
	}

	switch severity {
	case SeverityFatal:
		slog.Error("fatal error", args...)
	case SeverityError:
		slog.Error("error occurred", args...)
	case SeverityWarning:
		slog.Warn("warning occurred", args...)
	default:
		slog.Info("info occurred", args...)
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
