package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// writeErrorRouter 构造一个只用于测试 writeError 的路由。
func writeErrorRouter(err error) *route.Engine {
	router := route.NewEngine(config.NewOptions([]config.Option{}))
	router.GET("/probe", func(_ context.Context, _ *app.RequestContext) {})
	router.GET("/err", func(_ context.Context, c *app.RequestContext) { writeError(c, err) })
	return router
}

// decodeErrorBody 解析统一错误响应体。
func decodeErrorBody(t *testing.T, body []byte) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal error body %q: %v", body, err)
	}
	return got
}

// TestWriteErrorMapsCodes 验证错误码到 HTTP 状态码的映射。
func TestWriteErrorMapsCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   fault.Code
	}{
		{
			name:   "invalid_input → 400",
			err:    fault.New(fault.CodeInvalidInput, fault.LayerAPI, "bad input"),
			status: http.StatusBadRequest,
			code:   fault.CodeInvalidInput,
		},
		{
			name:   "not_found → 404",
			err:    fault.New(fault.CodeNotFound, fault.LayerAPI, "not found"),
			status: http.StatusNotFound,
			code:   fault.CodeNotFound,
		},
		{
			name:   "conflict → 409",
			err:    fault.New(fault.CodeConflict, fault.LayerAPI, "conflict"),
			status: http.StatusConflict,
			code:   fault.CodeConflict,
		},
		{
			name:   "model_unavailable → 503",
			err:    fault.New(fault.CodeModelUnavailable, fault.LayerInfrastructure, "模型不可用"),
			status: http.StatusServiceUnavailable,
			code:   fault.CodeModelUnavailable,
		},
		{
			name:   "internal → 500",
			err:    fault.New(fault.CodeInternal, fault.LayerRuntime, "boom"),
			status: http.StatusInternalServerError,
			code:   fault.CodeInternal,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := ut.PerformRequest(writeErrorRouter(c.err), "GET", "/err", nil)
			resp := w.Result()
			if resp.StatusCode() != c.status {
				t.Fatalf("status = %d, want %d, body=%s",
					resp.StatusCode(), c.status, resp.Body())
			}
			body := decodeErrorBody(t, resp.Body())
			if body.Code != string(c.code) {
				t.Fatalf("code = %q, want %q", body.Code, c.code)
			}
		})
	}
}

// TestWriteErrorHidesInternalDetail 验证底层错误不会泄漏给客户端。
func TestWriteErrorHidesInternalDetail(t *testing.T) {
	internal := errors.New("dial tcp 10.0.0.1:443: connection refused")
	err := fault.Wrap(fault.CodeModelUnavailable, fault.LayerInfrastructure,
		"模型服务暂时不可用", internal)

	w := ut.PerformRequest(writeErrorRouter(err), "GET", "/err", nil)
	resp := w.Result()

	body := decodeErrorBody(t, resp.Body())
	if body.Error != "模型服务暂时不可用" {
		t.Fatalf("message = %q", body.Error)
	}
	if strings.Contains(string(resp.Body()), "10.0.0.1") {
		t.Fatalf("internal detail leaked: %s", resp.Body())
	}
}

// TestWriteErrorUnwrappedPlainError 验证未包装错误统一 500 且不泄漏。
func TestWriteErrorUnwrappedPlainError(t *testing.T) {
	w := ut.PerformRequest(writeErrorRouter(errors.New("secret implementation detail")),
		"GET", "/err", nil)
	resp := w.Result()

	if resp.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode())
	}
	if strings.Contains(string(resp.Body()), "secret implementation detail") {
		t.Fatalf("plain error detail leaked: %s", resp.Body())
	}
	if body := decodeErrorBody(t, resp.Body()); body.Code != string(fault.CodeInternal) {
		t.Fatalf("code = %q", body.Code)
	}
}

// TestWriteErrorMapsStorageSentinels 验证存储层哨兵错误被正确映射。
func TestWriteErrorMapsStorageSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"storage.ErrNotFound → 404", storage.ErrNotFound, http.StatusNotFound},
		{"researchstate.ErrNotFound → 404", researchstate.ErrNotFound, http.StatusNotFound},
		{"researchstate.ErrNotAccepting → 409", researchstate.ErrNotAccepting, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := ut.PerformRequest(writeErrorRouter(c.err), "GET", "/err", nil)
			if resp := w.Result(); resp.StatusCode() != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode(), c.status)
			}
		})
	}
}

// TestWriteErrorIncludesSeverity 验证响应体带出严重级别。
func TestWriteErrorIncludesSeverity(t *testing.T) {
	err := fault.E(fault.CodeCommitFailed, fault.LayerResult, "结果保存失败").
		Severity(fault.SeverityFatal).Build()

	w := ut.PerformRequest(writeErrorRouter(err), "GET", "/err", nil)
	body := decodeErrorBody(t, w.Result().Body())
	if body.Severity != string(fault.SeverityFatal) {
		t.Fatalf("severity = %q, want fatal", body.Severity)
	}
}

// TestWriteErrorNil 验证 nil 不写响应。
func TestWriteErrorNil(t *testing.T) {
	router := route.NewEngine(config.NewOptions([]config.Option{}))
	router.GET("/ok", func(_ context.Context, c *app.RequestContext) {
		writeError(c, nil)
		c.JSON(http.StatusOK, map[string]string{"ok": "1"})
	})
	w := ut.PerformRequest(router, "GET", "/ok", nil)
	if resp := w.Result(); resp.StatusCode() != http.StatusOK {
		t.Fatalf("nil error should not override response, got %d", resp.StatusCode())
	}
}
