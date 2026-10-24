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
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/resultagg"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/scheduler"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/workpool"
)

// harness 装配 bus + Runtime + API，用于事件驱动的端到端测试。
type harness struct {
	api      *API
	rt       *runtime.Runtime
	runs     *memory.RunTaskStore
	tasks    *memory.TaskStore
	sessions *memory.SessionStore
	states   *stateMemory.Store
}

func newHarness() *harness {
	return newHarnessWith("mock")
}

// newHarnessWith 以指定模型提供方装配 harness。
//
// 装配真实的执行平面（Scheduler / WorkPool / ResultAggregator），
// 因为 Supervisor 通过 submit_assignments 派发的子任务需要它们执行与提交。
//
// "mock" 模型不调用任何工具，因此不会派发子任务、也不会调用 finish_research；
// 需要验证完整生命周期（派发 → 执行 → 结束）的测试应使用 "mock-tool"。
func newHarnessWith(provider string) *harness {
	sessions := memory.NewSessionStore()
	tasks := memory.NewTaskStore()
	runs := memory.NewRunTaskStore()
	states := stateMemory.NewStore()
	assignments := memory.NewAssignmentStore()
	works := memory.NewWorkStore()
	bus := event.NewBus()

	agg := resultagg.New(resultagg.Config{States: states, Bus: bus})
	pool := workpool.New(workpool.Config{
		Works: works, Bus: bus,
		Worker: workpool.NewMockWorker(5 * time.Millisecond), Workers: 2,
	})
	sched := scheduler.New(scheduler.Config{
		Assignments: assignments, Works: works, Pool: pool, Slots: agg,
		Bus: bus, MaxConcurrency: 2,
	})
	agg.SetSchedulerNotifier(sched)

	mdl, err := llm.NewSwappable(context.Background(), llm.Config{Provider: provider})
	if err != nil {
		panic(err)
	}
	rt := runtime.New(runs, tasks, states, mdl, bus)
	rt.SetExecution(assignments, works, sched, pool, agg)
	rt.Start()

	a := New(service.NewChatService(mdl), sessions, tasks,
		service.NewRunService(tasks, runs, states, bus), service.NewConfigService(mdl))
	a.SetExecutionStores(assignments, works)
	return &harness{api: a, rt: rt, runs: runs, tasks: tasks, sessions: sessions, states: states}
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

// TestCreateMultipleTasksAllowed 验证一个 Session 可创建多个 Task。
func TestCreateMultipleTasksAllowed(t *testing.T) {
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
	if resp = w.Result(); resp.StatusCode() != 201 {
		t.Fatalf("second task: want 201, got %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 列出应有 2 个 Task
	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sess.ID+"/tasks", nil)
	var list struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(w.Result().Body(), &list); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(list.Tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(list.Tasks))
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

	// 禁用 = 用户中止 -> stopped（终态）
	w = ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+taskID,
		jsonBody(t, map[string]bool{"enabled": false}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("disable: want 200, got %d, body=%s", resp.StatusCode(), resp.Body())
	}
	waitRunTask(t, h.runs, taskID, domain.RunStatusStopped)

	// 按 RunTaskID 查询，状态应为 stopped
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
	if rt2.Status != string(domain.RunStatusStopped) {
		t.Fatalf("want stopped, got %s", rt2.Status)
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

// TestResearchLifecycleFlow 验证 HTTP 层的研究生命周期：
// 启用 Task -> 发消息 -> 研究完成后 RunTask 置 Finished -> 再发消息自动新建 Task 与
// 新 ResearchState（继承上一份结论为 RelatedFinding）。
func TestResearchLifecycleFlow(t *testing.T) {
	// 用 mock-tool：它会调用 finish_research 宣布研究结束，
	// 从而验证"模型主动终结 → Finished → 下条消息新建 Task"的完整生命周期。
	h := newHarnessWith("mock-tool")
	defer h.stop(t)
	router := h.router(t)

	// 创建 Session + Task（description 作为研究需求）
	w := ut.PerformRequest(router, "POST", "/api/v1/sessions",
		jsonBody(t, map[string]string{"title": "s"}), jsonHeader())
	resp := w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create session: %d", resp.StatusCode())
	}
	var sess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp.Body(), &sess)

	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/tasks",
		jsonBody(t, map[string]string{"title": "t", "description": "研究某框架的架构"}), jsonHeader())
	resp = w.Result()
	if resp.StatusCode() != 201 {
		t.Fatalf("create task: %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var tk struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp.Body(), &tk)

	// 启用 Task：Runtime 初始化该 Task 的 ResearchState 并启动 SupervisorLoop。
	w = ut.PerformRequest(router, "PATCH", "/api/v1/tasks/"+tk.ID,
		jsonBody(t, map[string]bool{"enabled": true}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("enable: %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 发一条消息
	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/messages",
		jsonBody(t, map[string]string{"message": "请分析"}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 202 {
		t.Fatalf("send message: %d, body=%s", resp.StatusCode(), resp.Body())
	}

	// 轮询研究状态，直到出现研究结果
	waitResearchFindings(t, h.states, sess.ID, 1)

	st, err := h.states.GetLatestBySession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	// 研究需求来自用户消息（复数累积），而不是 Task 的 Title/Description。
	if len(st.Requirements) == 0 || st.Requirements[0] != "请分析" {
		t.Fatalf("requirements should start with the first user message, got %+v", st.Requirements)
	}
	// 过程输出 Finding 非空；模型通过 finish_research 给出的结论写入独立字段。
	if len(st.Findings) == 0 || st.Findings[0].Content == "" {
		t.Fatalf("findings = %+v", st.Findings)
	}
	if st.Conclusion == "" {
		t.Fatal("conclusion should be set after finish_research")
	}
	// 对话历史应记录 user + assistant 两条
	if len(st.History) != 2 ||
		st.History[0].Role != researchstate.RoleUser || st.History[0].Content != "请分析" ||
		st.History[1].Role != researchstate.RoleAssistant {
		t.Fatalf("history = %+v", st.History)
	}
	firstTaskID := st.TaskID

	// 等研究完成（RunTask -> Finished）
	waitRunStatus(t, h.runs, tk.ID, domain.RunStatusFinished)

	// 再次发消息：应自动新建 Task + 新 ResearchState（继承上一份结论）
	w = ut.PerformRequest(router, "POST", "/api/v1/sessions/"+sess.ID+"/messages",
		jsonBody(t, map[string]string{"message": "继续深入"}), jsonHeader())
	if resp = w.Result(); resp.StatusCode() != 202 {
		t.Fatalf("send second message: %d, body=%s", resp.StatusCode(), resp.Body())
	}

	waitTaskCount(t, h.tasks, sess.ID, 2)
	waitResearchFindings(t, h.states, sess.ID, 1)

	latest, err := h.states.GetLatestBySession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get latest state: %v", err)
	}
	if latest.TaskID == firstTaskID {
		t.Fatalf("should create a new ResearchState for the new Task, still %q", latest.TaskID)
	}
	if len(latest.RelatedNodes) == 0 {
		t.Fatalf("new ResearchState should carry RelatedNodes from previous task")
	}
	prevNode := latest.RelatedNodes[0]
	if prevNode.Ordinal != 1 {
		t.Fatalf("related node ordinal should be 1, got %d", prevNode.Ordinal)
	}
	if latest.Ordinal != 2 {
		t.Fatalf("new state ordinal should be 2, got %d", latest.Ordinal)
	}
	if len(prevNode.Findings) == 0 || prevNode.Findings[0].Content == "" {
		t.Fatalf("related node should carry previous finding, got %+v", prevNode.Findings)
	}
	if len(latest.Requirements) == 0 || latest.Requirements[0] != "继续深入" {
		t.Fatalf("new requirements should start with the follow-up message, got %+v", latest.Requirements)
	}

	// 对话历史接口应能回放两个任务的全部消息（每任务 user+assistant，共 4 条）
	w = ut.PerformRequest(router, "GET", "/api/v1/sessions/"+sess.ID+"/history", nil)
	if resp = w.Result(); resp.StatusCode() != 200 {
		t.Fatalf("get history: %d, body=%s", resp.StatusCode(), resp.Body())
	}
	var hist struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(resp.Body(), &hist); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(hist.Messages) != 4 {
		t.Fatalf("want 4 history messages, got %d: %+v", len(hist.Messages), hist.Messages)
	}
	if hist.Messages[0].Role != "user" || hist.Messages[0].Content != "请分析" {
		t.Fatalf("history[0] = %+v", hist.Messages[0])
	}
	if hist.Messages[2].Role != "user" || hist.Messages[2].Content != "继续深入" {
		t.Fatalf("history[2] = %+v", hist.Messages[2])
	}
}

func waitResearchFindings(t *testing.T, states *stateMemory.Store, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := states.GetLatestBySession(context.Background(), sessionID)
		if err == nil && len(st.Findings) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := states.GetLatestBySession(context.Background(), sessionID)
	t.Fatalf("want %d findings, got %+v", want, st)
}

// waitRunStatus 等待 RunTask 进入目标状态。
//
// 超时放宽到 10s：mock-tool 的完整链路包含"派发子任务 → 子任务执行 →
// 结果提交唤醒新一轮 → finish_research"，比纯 mock 更耗时。
func waitRunStatus(t *testing.T, runs *memory.RunTaskStore, taskID string, want domain.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := runs.GetByTask(context.Background(), taskID)
		if err == nil && rt.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rt, _ := runs.GetByTask(context.Background(), taskID)
	t.Fatalf("want run status %q, got %+v", want, rt)
}

func waitTaskCount(t *testing.T, tasks *memory.TaskStore, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, err := tasks.ListBySession(context.Background(), sessionID)
		if err == nil && len(list) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	list, _ := tasks.ListBySession(context.Background(), sessionID)
	t.Fatalf("want %d tasks, got %d", want, len(list))
}
