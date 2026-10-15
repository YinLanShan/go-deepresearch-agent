package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

func newAPI() *API {
	return New(
		service.NewChatService(mock.New()),
		memory.NewSessionStore(),
		memory.NewTaskStore(),
	)
}

// newTestRouter 构造一个内存路由，用于测试非 SSE 的 JSON 端点。
func newTestRouter(t *testing.T) *route.Engine {
	t.Helper()
	router := route.NewEngine(config.NewOptions([]config.Option{}))
	newAPI().Register(router)
	return router
}

func jsonBody(t *testing.T, v any) *ut.Body {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &ut.Body{Body: strings.NewReader(string(b)), Len: len(b)}
}

// TestChatSSE 通过真实 HTTP 连接验证模型结果经 SSE 流式返回。
// 说明：SSE 依赖 hijack/chunked writer，需要真实网络连接，
// 因此不使用 ut.PerformRequest（其内存环境无网络 writer）。
func TestChatSSE(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h := server.New(server.WithListener(ln), server.WithSenseClientDisconnection(true))
	newAPI().Register(h)
	go func() { _ = h.Run() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	}()

	req, err := http.NewRequest(http.MethodPost,
		"http://"+ln.Addr().String()+"/api/v1/chat",
		strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("want text/event-stream, got %q", ct)
	}
	s := string(body)
	for _, want := range []string{
		"event: message",
		`data: {"delta":"mock rep"}`,
		"event: done",
		`data: {"status":"completed"}`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %q:\n%s", want, s)
		}
	}
}

func TestChatRejectsEmptyMessage(t *testing.T) {
	router := newTestRouter(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/chat",
		jsonBody(t, map[string]string{"message": ""}),
		ut.Header{Key: "Content-Type", Value: "application/json"},
	)
	if resp := w.Result(); resp.StatusCode() != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode())
	}
}

func TestSessionAndTaskLifecycle(t *testing.T) {
	router := newTestRouter(t)

	// 创建 Session
	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "my session"}),
		ut.Header{Key: "Content-Type", Value: "application/json"},
	)
	resp := w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create session: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &sess); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("session id is empty")
	}

	// 在 Session 下创建 Task
	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "task title", "description": "desc"}),
		ut.Header{Key: "Content-Type", Value: "application/json"},
	)
	resp = w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create task: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 列出该 Session 的 Task
	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sess.ID+"/tasks", nil)
	resp = w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("list tasks: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var listed struct {
		Tasks []struct {
			Title string `json:"title"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Body(), &listed); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(listed.Tasks) != 1 || listed.Tasks[0].Title != "task title" {
		t.Fatalf("unexpected tasks: %+v", listed.Tasks)
	}
}

func TestCreateTaskUnderMissingSession(t *testing.T) {
	router := newTestRouter(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions/missing/tasks",
		jsonBody(t, map[string]string{"title": "t"}),
		ut.Header{Key: "Content-Type", Value: "application/json"},
	)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode())
	}
}

// createSessionAndTask 创建一个 Session 并在其下创建一个 Task，返回两者 ID。
func createSessionAndTask(t *testing.T, router *route.Engine) (sessionID, taskID string) {
	t.Helper()
	jsonHdr := ut.Header{Key: "Content-Type", Value: "application/json"}

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "s"}), jsonHdr)
	resp := w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create session: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &sess); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}

	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "t"}), jsonHdr)
	resp = w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create task: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var tk struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &tk); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	return sess.ID, tk.ID
}

func TestCreateSecondTaskConflict(t *testing.T) {
	router := newTestRouter(t)
	jsonHdr := ut.Header{Key: "Content-Type", Value: "application/json"}

	// 创建 Session + 第一个 Task
	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "s"}), jsonHdr)
	resp := w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create session: want 201, got %d", resp.StatusCode())
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &sess); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}

	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "t1"}), jsonHdr)
	if resp = w.Result(); resp.StatusCode() != 201 {
		t.Fatalf("first task: want 201, got %d", resp.StatusCode())
	}

	// 同一 Session 下创建第二个 Task 应返回 409
	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "t2"}), jsonHdr)
	if resp = w.Result(); resp.StatusCode() != 409 {
		t.Fatalf("second task: want 409, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
}

func TestDeleteTask(t *testing.T) {
	router := newTestRouter(t)
	sessID, taskID := createSessionAndTask(t, router)

	w := ut.PerformRequest(router, "DELETE", "/api/v1/sessions/"+sessID+"/tasks/"+taskID, nil)
	if resp := w.Result(); resp.StatusCode() != 204 {
		t.Fatalf("delete task: want 204, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 再次删除应 404
	w = ut.PerformRequest(router, "DELETE", "/api/v1/sessions/"+sessID+"/tasks/"+taskID, nil)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("double delete task: want 404, got %d", resp.StatusCode())
	}

	// 列表应为空
	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sessID+"/tasks", nil)
	resp := w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("list tasks: want 200, got %d", resp.StatusCode())
	}
	var listed struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Body(), &listed); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(listed.Tasks) != 0 {
		t.Fatalf("want empty list, got %d tasks", len(listed.Tasks))
	}
}

func TestDeleteSessionCascade(t *testing.T) {
	router := newTestRouter(t)
	sessID, _ := createSessionAndTask(t, router)

	w := ut.PerformRequest(router, "DELETE", "/api/v1/sessions/"+sessID, nil)
	if resp := w.Result(); resp.StatusCode() != 204 {
		t.Fatalf("delete session: want 204, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 级联后，该 Session 下的 Task 列表应为空
	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sessID+"/tasks", nil)
	resp := w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("list tasks after cascade: want 200, got %d", resp.StatusCode())
	}
	var listed struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Body(), &listed); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(listed.Tasks) != 0 {
		t.Fatalf("want empty list after cascade, got %d tasks", len(listed.Tasks))
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	router := newTestRouter(t)

	w := ut.PerformRequest(router, "DELETE", "/api/v1/sessions/missing", nil)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode())
	}
}
