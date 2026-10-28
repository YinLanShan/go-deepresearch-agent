package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
)

// flakyModel 前 n 次返回指定错误，之后成功。
type flakyModel struct {
	failures int32
	err      error
	calls    int32
}

func (m *flakyModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	n := atomic.AddInt32(&m.calls, 1)
	if n <= m.failures {
		return nil, m.err
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (m *flakyModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()
	return sr, nil
}

// TestRetryOnRateLimited 验证限流错误会重试并最终成功。
func TestRetryOnRateLimited(t *testing.T) {
	inner := &flakyModel{
		failures: 2,
		err:      fault.New(fault.CodeRateLimited, fault.LayerInfrastructure, "rate limited"),
	}
	m := withRetry(inner, Config{MaxRetries: 3, RetryInterval: time.Millisecond})

	out, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if out.Content != "ok" {
		t.Fatalf("content = %q", out.Content)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 3 {
		t.Fatalf("want 3 calls (1 + 2 retries), got %d", got)
	}
}

// TestNoRetryOnNonRetryable 验证不可重试错误立即返回。
func TestNoRetryOnNonRetryable(t *testing.T) {
	inner := &flakyModel{
		failures: 99,
		err:      fault.New(fault.CodeInvalidInput, fault.LayerInfrastructure, "bad request"),
	}
	m := withRetry(inner, Config{MaxRetries: 3, RetryInterval: time.Millisecond})

	if _, err := m.Generate(context.Background(), nil); err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Fatalf("non-retryable should not retry, got %d calls", got)
	}
}

// TestRetryExhaustedReturnsModelUnavailable 验证重试耗尽后的错误码。
func TestRetryExhaustedReturnsModelUnavailable(t *testing.T) {
	inner := &flakyModel{
		failures: 99,
		err:      fault.New(fault.CodeRateLimited, fault.LayerInfrastructure, "rate limited"),
	}
	m := withRetry(inner, Config{MaxRetries: 2, RetryInterval: time.Millisecond})

	_, err := m.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("want error")
	}
	if got := fault.CodeOf(err); got != fault.CodeModelUnavailable {
		t.Fatalf("code = %s, want model_unavailable", got)
	}
	// 1 次初始 + 2 次重试
	if got := atomic.LoadInt32(&inner.calls); got != 3 {
		t.Fatalf("want 3 attempts, got %d", got)
	}
}

// TestRetryRespectsContext 验证 ctx 取消后不再重试。
func TestRetryRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inner := &flakyModel{
		failures: 99,
		err:      fault.New(fault.CodeRateLimited, fault.LayerInfrastructure, "rate limited"),
	}
	m := withRetry(inner, Config{MaxRetries: 3, RetryInterval: time.Second})

	if _, err := m.Generate(ctx, nil); err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Fatalf("cancelled ctx should not retry, got %d calls", got)
	}
}

// TestNoRetryWhenDisabled 验证 MaxRetries=0 时原样返回（不包装）。
func TestNoRetryWhenDisabled(t *testing.T) {
	inner := &flakyModel{}
	m := withRetry(inner, Config{MaxRetries: 0})
	if m != model.BaseChatModel(inner) {
		t.Fatal("MaxRetries=0 should return the original model unwrapped")
	}
}

// TestShouldRetryPlainError 验证未包装错误按文本粗判。
func TestShouldRetryPlainError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"HTTP 429: too many requests", true},
		{"service unavailable (503)", true},
		{"connection reset by peer", true},
		{"invalid api key", false},
		{"model not found", false},
	}
	for _, c := range cases {
		got := shouldRetry(context.Background(), errors.New(c.msg))
		if got != c.want {
			t.Fatalf("shouldRetry(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
