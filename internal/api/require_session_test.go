package api

import (
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

// TestRequireSessionHelpers 集中验证前置校验辅助方法的错误语义。
//
// 抽取 requireSessionID / requireSession 后，各 handler 的 400/404 行为
// 由这两个方法统一保证，这里固定住它们的契约。
func TestRequireSessionHelpers(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	// 需要 Session 存在的接口：不存在时统一 404。
	notFoundCases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "创建 Task", method: "POST", path: "/api/v1/sessions/no-such/tasks", body: map[string]string{"title": "t"}},
		{name: "删除 Session", method: "DELETE", path: "/api/v1/sessions/no-such"},
		{name: "投递消息", method: "POST", path: "/api/v1/sessions/no-such/messages", body: map[string]string{"message": "hi"}},
		{name: "SSE 研究", method: "POST", path: "/api/v1/sessions/no-such/research", body: map[string]string{"message": "hi"}},
	}
	for _, c := range notFoundCases {
		t.Run(c.name+" → 404", func(t *testing.T) {
			var w *ut.ResponseRecorder
			if c.body != nil {
				w = ut.PerformRequest(router, c.method, c.path, jsonBody(t, c.body), jsonHeader())
			} else {
				w = ut.PerformRequest(router, c.method, c.path, nil)
			}
			if resp := w.Result(); resp.StatusCode() != http.StatusNotFound {
				t.Fatalf("want 404, got %d, body=%s", resp.StatusCode(), resp.Body())
			}
		})
	}

	// 仅按 ID 查询的接口：Session 不存在时不校验存在性，由底层返回各自结果。
	// 这里只确认不会因"缺少 Session"直接 500。
	queryCases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "研究状态", method: "GET", path: "/api/v1/sessions/no-such/research-state"},
		{name: "对话历史", method: "GET", path: "/api/v1/sessions/no-such/history"},
		{name: "后台任务", method: "GET", path: "/api/v1/sessions/no-such/background-tasks"},
	}
	for _, c := range queryCases {
		t.Run(c.name+" 不因缺少 Session 报 500", func(t *testing.T) {
			w := ut.PerformRequest(router, c.method, c.path, nil)
			if resp := w.Result(); resp.StatusCode() == http.StatusInternalServerError {
				t.Fatalf("unexpected 500, body=%s", resp.Body())
			}
		})
	}
}
