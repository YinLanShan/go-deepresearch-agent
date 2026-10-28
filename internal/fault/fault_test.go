package fault

import (
	"context"
	"errors"
	"testing"
)

// TestWrapIsIdempotent 验证跨层包装不会覆盖内层判断。
//
// 这是错误分层的关键：最了解情况的那一层决定分类，
// 外层（如 Runtime）不应把 rate_limited 重新标成 internal。
func TestWrapIsIdempotent(t *testing.T) {
	inner := E(CodeRateLimited, LayerInfrastructure, "upstream limited").Build()

	outer := Wrap(CodeInternal, LayerRuntime, "runtime failed", inner)

	if outer != inner {
		t.Fatalf("wrap should return the existing error, got %+v", outer)
	}
	if outer.Code != CodeRateLimited {
		t.Fatalf("code should stay rate_limited, got %s", outer.Code)
	}
	if outer.Layer != LayerInfrastructure {
		t.Fatalf("layer should stay infrastructure, got %s", outer.Layer)
	}
}

// TestWrapNil 验证 nil 输入不产生错误。
func TestWrapNil(t *testing.T) {
	if got := Wrap(CodeInternal, LayerRuntime, "x", nil); got != nil {
		t.Fatalf("wrap(nil) should be nil, got %+v", got)
	}
	if got := E(CodeInternal, LayerRuntime, "x").Wrap(nil); got != nil {
		t.Fatalf("builder.Wrap(nil) should be nil, got %+v", got)
	}
}

// TestBuilderFields 验证链式构造的各字段。
func TestBuilderFields(t *testing.T) {
	err := E(CodeCommitFailed, LayerResult, "commit failed").
		Severity(SeverityFatal).
		Source("resultagg.commit").
		IDs("t1", "r1", "a1", "w1").
		Retryable(false).
		Wrap(errors.New("disk full"))

	if err.Code != CodeCommitFailed || err.Layer != LayerResult {
		t.Fatalf("identity fields wrong: %+v", err)
	}
	if err.Severity != SeverityFatal {
		t.Fatalf("severity = %s", err.Severity)
	}
	if err.Source != "resultagg.commit" {
		t.Fatalf("source = %s", err.Source)
	}
	if err.TaskID != "t1" || err.RunTaskID != "r1" || err.AssignmentID != "a1" || err.WorkID != "w1" {
		t.Fatalf("scope ids wrong: %+v", err)
	}
	if err.RetryableHint {
		t.Fatal("retryable should be false")
	}
	if err.OccurredAt.IsZero() {
		t.Fatal("occurredAt should be set")
	}
	// 底层错误仍可被 errors.Is 命中
	if !errors.Is(err, err.Err) {
		t.Fatal("unwrap chain broken")
	}
}

// TestDefaultSeverity 验证默认级别为 error。
func TestDefaultSeverity(t *testing.T) {
	err := New(CodeInternal, LayerRuntime, "x")
	if err.Severity != SeverityError {
		t.Fatalf("default severity = %s, want error", err.Severity)
	}
}

// TestSeverityOf 验证未包装错误按 error 处理。
func TestSeverityOf(t *testing.T) {
	plain := errors.New("boom")
	if got := SeverityOf(plain); got != SeverityError {
		t.Fatalf("plain error severity = %s, want error", got)
	}
	wrapped := E(CodeWorkTimeout, LayerWork, "timeout").Severity(SeverityWarning).Build()
	if got := SeverityOf(wrapped); got != SeverityWarning {
		t.Fatalf("wrapped severity = %s, want warning", got)
	}
}

// TestClientMessage 验证客户端只拿到面向用户的说明。
func TestClientMessage(t *testing.T) {
	err := Wrap(CodeModelUnavailable, LayerInfrastructure, "模型服务暂时不可用", errors.New("dial tcp: refused"))
	if got := err.ClientMessage(); got != "模型服务暂时不可用" {
		t.Fatalf("client message = %q", got)
	}
	// 完整信息仍保留在 Error() 中供日志使用
	if err.Error() == err.ClientMessage() {
		t.Fatal("Error() should contain more detail than ClientMessage()")
	}
}

// TestCodeOfAndLayerOf 验证提取函数。
func TestCodeOfAndLayerOf(t *testing.T) {
	err := New(CodeNotFound, LayerAPI, "x")
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("code = %s", CodeOf(err))
	}
	if LayerOf(err) != LayerAPI {
		t.Fatalf("layer = %s", LayerOf(err))
	}

	plain := errors.New("plain")
	if CodeOf(plain) != "" || LayerOf(plain) != "" {
		t.Fatal("plain error should yield empty code/layer")
	}
}

// TestRetryOnceSucceedsAfterFailure 验证首次失败后重试成功。
func TestRetryOnceSucceedsAfterFailure(t *testing.T) {
	calls := 0
	err := RetryOnce(context.Background(), func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

// TestRetryOnceDoesNotRetryNotFound 验证确定性错误不重试。
func TestRetryOnceDoesNotRetryNotFound(t *testing.T) {
	calls := 0
	err := RetryOnce(context.Background(), func() error {
		calls++
		return ErrNotFound
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("not-found should not be retried, got %d calls", calls)
	}
}

// TestRetryOnceRespectsContext 验证 ctx 取消后不重试。
func TestRetryOnceRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	err := RetryOnce(ctx, func() error {
		calls++
		return errors.New("transient")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("cancelled ctx should not retry, got %d calls", calls)
	}
}

// TestRetryOnceOnlyTwice 验证最多执行两次。
func TestRetryOnceOnlyTwice(t *testing.T) {
	calls := 0
	err := RetryOnce(context.Background(), func() error {
		calls++
		return errors.New("always")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if calls != 2 {
		t.Fatalf("want exactly 2 attempts, got %d", calls)
	}
}

// TestIsNotFound 验证带包装链的 not-found 判断。
func TestIsNotFound(t *testing.T) {
	sentinel := errors.New("storage: not found")
	wrapped := Wrap(CodeNotFound, LayerRuntime, "x", sentinel)

	if !IsNotFound(wrapped, sentinel) {
		t.Fatal("should detect not-found through wrap chain")
	}
	if IsNotFound(errors.New("other")) {
		t.Fatal("unrelated error should not be not-found")
	}
	if IsNotFound(nil) {
		t.Fatal("nil should not be not-found")
	}
}
