package api

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

// harness 装配 bus + Runtime + API，用于事件驱动的端到端测试。
type harness struct {
	api      *API
	rt       *runtime.Runtime
	runs     *memory.RunTaskStore
	tasks    *memory.TaskStore
	sessions *memory.SessionStore
}

func newHarness() *harness {
	sessions := memory.NewSessionStore()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	bus := event.NewBus()
	runtime.RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")
	rt := runtime.New(runs, bus)
	rt.Start()
	a := New(service.NewChatService(mock.New()), sessions, tasks, service.NewRunService(tasks, runs, bus))
	return &harness{api: a, rt: rt, runs: runs, tasks: tasks, sessions: sessions}
}

func (h *harness) router(t *testing.T) *route.Engine {
	t.Helper()
	router := route.NewEngine(config.NewOptions([]config.Option{}))
	h.api.Register(router)
	return router
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.rt.Shutdown(ctx)
}

func jsonBody(t *testing.T, v any) *ut.Body {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &ut.Body{Body: strings.NewReader(string(b)), Len: len(b)}
}

func jsonHeader() ut.Header {
	return ut.Header{Key: "Content-Type", Value: "application/json"}
}

// waitRunTask 轮询等待 RunTask 达到指定状态（事件由 Runtime 异步处理）。
func waitRunTask(t *testing.T, runs *memory.RunTaskStore, taskID string, want domain.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := runs.GetByTask(context.Background(), taskID)
		if err == nil && rt.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt, _ := runs.GetByTask(context.Background(), taskID)
	t.Fatalf("run task not in state %q, got %+v", want, rt)
}

// createSessionAndTask 创建一个 Session 并在其下创建一个 Task，返回两者 ID。
func createSessionAndTask(t *testing.T, router *route.Engine) (sessionID, taskID string) {
	t.Helper()

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "s"}), jsonHeader())
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
		jsonBody(t, map[string]string{"title": "t"}), jsonHeader())
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

// createRunTask 在存储中直接创建一个 RunTask，用于设置"已存在"前置条件。
// 生产环境 RunTask 由后续独立的创建流程产生；本阶段测试用该方法预置。
func (h *harness) createRunTask(t *testing.T, taskID string) string {
	t.Helper()
	rt := &domain.RunTask{ID: ids.New(), TaskID: taskID, Status: domain.RunStatusCreated}
	if err := h.runs.Create(context.Background(), rt); err != nil {
		t.Fatalf("create run task: %v", err)
	}
	return rt.ID
}

// TestChatSSE 通过真实 HTTP 连接验证模型结果经 SSE 流式返回。
func TestChatSSE(t *testing.T) {
	h := newHarness()
	defer h.stop(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := server.New(server.WithListener(ln), server.WithSenseClientDisconnection(true))
	h.api.Register(srv)
	go func() { _ = srv.Run() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
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
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/chat",
		jsonBody(t, map[string]string{"message": ""}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode())
	}
}

func TestSessionAndTaskLifecycle(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "my session"}), jsonHeader())
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
		jsonBody(t, map[string]string{"title": "task title", "description": "desc"}), jsonHeader())
	resp = w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create task: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sess.ID+"/tasks", nil)
	resp = w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("list tasks: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var listed struct {
		Tasks []struct {
			Title   string `json:"title"`
			Enabled bool   `json:"enabled"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(resp.Body(), &listed); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(listed.Tasks) != 1 || listed.Tasks[0].Title != "task title" {
		t.Fatalf("unexpected tasks: %+v", listed.Tasks)
	}
	if listed.Tasks[0].Enabled {
		t.Fatal("task should be disabled by default")
	}
}

func TestCreateTaskUnderMissingSession(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions/missing/tasks",
		jsonBody(t, map[string]string{"title": "t"}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode())
	}
}

func TestCreateSecondTaskConflict(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "s"}), jsonHeader())
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
		jsonBody(t, map[string]string{"title": "t1"}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 201 {
		t.Fatalf("first task: want 201, got %d", resp.StatusCode())
	}

	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "t2"}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 409 {
		t.Fatalf("second task: want 409, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
}

func TestDeleteSessionNotFound(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)

	w := ut.PerformRequest(router, "DELETE", "/api/v1/sessions/missing", nil)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode())
	}
}

// TestEnableDisableRunTaskFlow 验证协商式的 RunTask 生命周期：
// 启用 Task -> Runtime 协商后置 running；禁用 -> 协商后 paused；查询一致。
func TestEnableDisableRunTaskFlow(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)
	_, taskID := createSessionAndTask(t, router)
	h.createRunTask(t, taskID) // 前置：RunTask 已存在（enable 不负责创建）

	// 启用
	w := ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": true}), jsonHeader())
	resp := w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("enable: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	waitRunTask(t, h.runs, taskID, domain.RunStatusRunning)

	// 按 TaskID 查询 RunTask
	w = ut.PerformRequest(router, "GET", "/api/v1/tasks/"+taskID+"/run", nil)
	resp = w.Result()
	if resp.StatusCode() != 200 {
		t.Fatalf("get run by task: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var rt struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &rt); err != nil {
		t.Fatalf("unmarshal run task: %v", err)
	}

	// 禁用 -> paused
	w = ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": false}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("disable: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	waitRunTask(t, h.runs, taskID, domain.RunStatusPaused)

	// 按 RunTaskID 查询，状态应为 paused
	w = ut.PerformRequest(router, "GET", "/api/v1/run-tasks/"+rt.ID, nil)
	if resp = w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("get run by id: want 200, got %d", resp.StatusCode())
	}
	var rt2 struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(resp.Body(), &rt2); err != nil {
		t.Fatalf("unmarshal run task: %v", err)
	}
	if rt2.Status != string(domain.RunStatusPaused) {
		t.Fatalf("want paused, got %s", rt2.Status)
	}
}

func TestGetRunNotFound(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)
	_, taskID := createSessionAndTask(t, router)

	w := ut.PerformRequest(router, "GET", "/api/v1/tasks/"+taskID+"/run", nil)
	if resp := w.Result(); resp.StatusCode() != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode())
	}
}

// TestDeleteSessionCascadesRunTask 验证 Session 删除级联删除 Task 与 RunTask，
// 且 RunTask 的删除经 Runtime 协商后完成。
func TestDeleteSessionCascadesRunTask(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)
	sessID, taskID := createSessionAndTask(t, router)
	h.createRunTask(t, taskID) // 前置：RunTask 已存在

	// 删除 Session -> 级联删除 Task 与 RunTask
	w := ut.PerformRequest(router, "DELETE", "/api/v1/sessions/"+sessID, nil)
	if resp := w.Result(); resp.StatusCode() != 204 {
		t.Fatalf("delete session: want 204, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// RunTask 已被级联删除
	if _, err := h.runs.GetByTask(context.Background(), taskID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("run task should be cascade-deleted, got %v", err)
	}
}

// TestEnableCreatesRunTaskWhenMissing 未关联 RunTask 的 Task 使能时，
// Runtime 会协商创建 RunTask 并置为 running。
func TestEnableCreatesRunTaskWhenMissing(t *testing.T) {
	h := newHarness()
	defer h.stop(t)
	router := h.router(t)
	_, taskID := createSessionAndTask(t, router) // 未创建 RunTask

	w := ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": true}), jsonHeader())
	if resp := w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("enable: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	waitRunTask(t, h.runs, taskID, domain.RunStatusRunning)
}
