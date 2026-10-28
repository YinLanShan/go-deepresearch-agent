package llm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
)

// DefaultRetryInterval 是模型重试的默认固定间隔。
const DefaultRetryInterval = 2 * time.Second

// retryableCodes 是"值得重试"的模型错误码。
//
// 只重试限流与服务不可用：这两类是瞬时的外部状态，
// 其余错误（参数错误、鉴权失败、模型不存在）重试没有意义。
var retryableCodes = map[fault.Code]struct{}{
	fault.CodeRateLimited:      {},
	fault.CodeModelUnavailable: {},
	fault.CodeUnavailable:      {},
}

// retryModel 包装一个 ChatModel，在可重试错误上做固定间隔重试（Q1=A）。
//
// 语义：
//   - 仅对 rate_limited / model_unavailable / unavailable 重试；
//   - 间隔固定（默认 2s），不递增；
//   - 次数用完仍失败 → 原样返回错误，由上层走"研究失败"流程
//     （上报前端 → 清理实体 → 失能 RunTask → 返回 error）。
type retryModel struct {
	inner    model.BaseChatModel
	maxRetry int
	interval time.Duration
}

// 编译期断言。
var _ model.BaseChatModel = (*retryModel)(nil)

// withRetry 按配置包装模型；maxRetry<=0 时原样返回（不引入额外开销）。
func withRetry(inner model.BaseChatModel, cfg Config) model.BaseChatModel {
	if inner == nil || cfg.MaxRetries <= 0 {
		return inner
	}
	interval := cfg.RetryInterval
	if interval <= 0 {
		interval = DefaultRetryInterval
	}
	return &retryModel{inner: inner, maxRetry: cfg.MaxRetries, interval: interval}
}

// Generate 带重试的非流式调用。
func (m *retryModel) Generate(ctx context.Context, in []*schema.Message,
	opts ...model.Option) (*schema.Message, error) {
	var lastErr error
	for attempt := 0; attempt <= m.maxRetry; attempt++ {
		if attempt > 0 {
			slog.Warn("llm: retrying model call",
				"attempt", attempt, "max", m.maxRetry, "interval", m.interval)
			if err := sleepCtx(ctx, m.interval); err != nil {
				return nil, err
			}
		}
		out, err := m.inner.Generate(ctx, in, opts...)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !shouldRetry(ctx, err) {
			return nil, err
		}
	}
	return nil, exhaustError(lastErr, m.maxRetry)
}

// Stream 带重试的流式调用（仅构造流失败时重试；已开始的流无法重试）。
func (m *retryModel) Stream(ctx context.Context, in []*schema.Message,
	opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	var lastErr error
	for attempt := 0; attempt <= m.maxRetry; attempt++ {
		if attempt > 0 {
			slog.Warn("llm: retrying model stream",
				"attempt", attempt, "max", m.maxRetry, "interval", m.interval)
			if err := sleepCtx(ctx, m.interval); err != nil {
				return nil, err
			}
		}
		sr, err := m.inner.Stream(ctx, in, opts...)
		if err == nil {
			return sr, nil
		}
		lastErr = err
		if !shouldRetry(ctx, err) {
			return nil, err
		}
	}
	return nil, exhaustError(lastErr, m.maxRetry)
}

// shouldRetry 判断错误是否值得重试。
func shouldRetry(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// ctx 已结束：重试没有意义。
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		_, ok := retryableCodes[fe.Code]
		return ok
	}
	// 未包装错误：按文本粗判限流/不可用（兼容第三方客户端直接返回的错误）。
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"429", "rate limit", "too many requests",
		"503", "502", "504", "unavailable", "overloaded", "timeout", "connection reset"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// exhaustError 在重试耗尽后返回带上下文的错误。
func exhaustError(lastErr error, maxRetry int) error {
	if lastErr == nil {
		return nil
	}
	return fault.E(fault.CodeModelUnavailable, fault.LayerInfrastructure,
		"model call failed after retries").
		Severity(fault.SeverityError).
		Source("llm.retry").
		Retryable(false).
		WrapForce(lastErr)
}

// sleepCtx 等待固定间隔，可被 ctx 提前打断。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
