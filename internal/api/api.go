// Package api 是 API Layer，基于 Hertz 提供 HTTP 接口与 SSE 事件输出。
//
// 对应 docs/Architecture.md 6.2 节 API Layer 的定位：负责请求校验与事件输出，
// 不参与研究决策。阶段 0 提供：
//   - POST /api/v1/chat           —— 调用模型并通过 SSE 流式返回结果
//   - POST /api/v1/sessions       —— 创建 Session
//   - POST /api/v1/sessions/:id/tasks —— 在 Session 下创建 Task
//   - GET  /api/v1/sessions/:id/tasks —— 列出 Session 下的 Task
//   - DELETE /api/v1/sessions/:id      —— 删除 Session（级联删除 Task 与 RunTask）
//   - PATCH /api/v1/tasks/:id          —— 启用/禁用 Task（经事件驱动 Runtime）
//   - GET  /api/v1/tasks/:taskID/run   —— 查询 Task 对应的 RunTask
//   - GET  /api/v1/run-tasks/:runTaskID —— 按 ID 查询 RunTask
//
// 约束：
//   - 一个 Session 只对应一个 Task，重复创建返回 409。
//   - 一个 Task 最多对应一个 RunTask，Task 只能启用/禁用，无直接删除路径。
//   - RunTask 由独立实体 Runtime 经事件订阅维护，不由 HTTP Handler 直接变更；
//     启用/禁用/删除都经 Runtime 协商依赖组件后原子落地。
//   - 使能时若 RunTask 不存在，Runtime 先协商创建（含初始化要求）再新建 RunTask。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/sse"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/ids"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// API 聚合各层依赖，暴露 HTTP 处理器。
type API struct {
	chat     *service.ChatService
	sessions storage.SessionStore
	tasks    storage.TaskStore
	runs     *service.RunService
	config   *service.ConfigService
	now      func() time.Time

	// 后台任务（Assignment / Work）的数据来源，由 main 装配注入。
	assignments storage.AssignmentStore
	works       storage.WorkStore
}

// New 构造一个 API 实例。
func New(chat *service.ChatService, sessions storage.SessionStore, tasks storage.TaskStore,
	runs *service.RunService, cfg *service.ConfigService) *API {
	return &API{chat: chat, sessions: sessions, tasks: tasks, runs: runs, config: cfg, now: time.Now}
}

// SetExecutionStores 注入后台任务（Assignment / Work）存储。
func (a *API) SetExecutionStores(assignments storage.AssignmentStore, works storage.WorkStore) {
	a.assignments = assignments
	a.works = works
}

// ─── 请求前置校验 ────────────────────────────────────────────────────────────

// requireSessionID 读取路径参数 :id（Session ID）。
//
// 参数为空时直接写出 400 响应并返回 ok=false，调用方应立即返回。
func (a *API) requireSessionID(c *app.RequestContext) (string, bool) {
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return "", false
	}
	return sessionID, true
}

// requireSession 读取路径参数 :id 并确认该 Session 存在。
//
// 参数为空写出 400，Session 不存在写出 404，其他错误写出 500；
// 任一失败返回 ok=false，调用方应立即返回。
//
// 用于需要"操作目标 Session"的接口（创建 Task、投递消息、SSE 订阅等）；
// 仅按 ID 查询的接口（列出 Task、查询研究状态）使用 requireSessionID 即可。
func (a *API) requireSession(ctx context.Context, c *app.RequestContext) (string, bool) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return "", false
	}
	if _, err := a.sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return "", false
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return "", false
	}
	return sessionID, true
}

// Register 在路由上注册全部 HTTP 接口。
// 参数使用 route.IRouter，兼容生产环境 *server.Hertz 与测试环境 *route.Engine。
func (a *API) Register(r route.IRouter) {
	v1 := r.Group("/api/v1")
	v1.POST("/chat", a.handleChat)
	v1.POST("/sessions", a.handleCreateSession)
	v1.GET("/sessions", a.handleListSessions)
	v1.POST("/sessions/:id/tasks", a.handleCreateTask)
	v1.GET("/sessions/:id/tasks", a.handleListTasks)
	v1.DELETE("/sessions/:id", a.handleDeleteSession)
	v1.PATCH("/tasks/:id", a.handleUpdateTask)
	v1.GET("/tasks/:taskID/run", a.handleGetRunByTask)
	v1.GET("/run-tasks/:runTaskID", a.handleGetRunTask)
	v1.POST("/sessions/:id/messages", a.handleSendMessage)
	v1.GET("/sessions/:id/research-state", a.handleGetResearchState)
	v1.GET("/sessions/:id/history", a.handleGetHistory)
	v1.POST("/sessions/:id/stop", a.handleStop)
	v1.POST("/sessions/:id/approve", a.handleApprove)
	v1.POST("/sessions/:id/research", a.handleResearchStream)
	// 会话级常驻 SSE：状态徽章与后台任务面板的实时数据源。
	// 与 /research 不同，它不绑定某一次研究请求，研究在后台推进时同样推送。
	v1.GET("/sessions/:id/events", a.handleSessionEvents)
	// 后台任务（Assignment + Work）：侧边栏实时展示子 Agent 执行情况。
	v1.GET("/sessions/:id/background-tasks", a.handleBackgroundTasks)
	v1.GET("/config", a.handleGetConfig)
	v1.PUT("/config", a.handleUpdateConfig)
}

type chatRequest struct {
	Message string `json:"message"`
}

// handleChat 调用模型并通过 SSE 流式返回结果。
func (a *API) handleChat(ctx context.Context, c *app.RequestContext) {
	var req chatRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	w := sse.NewWriter(c)
	streamErr := a.chat.Stream(ctx, req.Message, func(delta string) error {
		payload, _ := json.Marshal(map[string]string{"delta": delta})
		return w.WriteEvent("", "message", payload)
	})

	if streamErr != nil {
		payload, _ := json.Marshal(map[string]string{"error": streamErr.Error()})
		_ = w.WriteEvent("", "error", payload)
	}
	done, _ := json.Marshal(map[string]string{"status": "completed"})
	_ = w.WriteEvent("", "done", done)
	_ = w.Close()
}

type sessionRequest struct {
	Title string `json:"title"`
}

func (a *API) handleCreateSession(_ context.Context, c *app.RequestContext) {
	var req sessionRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	now := a.now()
	sess := &domain.Session{ID: ids.New(), Title: req.Title, CreatedAt: now, UpdatedAt: now}
	if err := a.sessions.Create(context.Background(), sess); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sess)
}

// handleListSessions 列出全部 Session，供前端侧边栏使用。
func (a *API) handleListSessions(ctx context.Context, c *app.RequestContext) {
	sessions, err := a.sessions.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []*domain.Session{}
	}
	c.JSON(http.StatusOK, sessions)
}

// handleGetConfig 返回当前模型配置视图（密钥脱敏）。
func (a *API) handleGetConfig(_ context.Context, c *app.RequestContext) {
	if a.config == nil {
		c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "config service unavailable"})
		return
	}
	c.JSON(http.StatusOK, a.config.Get())
}

// handleUpdateConfig 热切换模型配置并立即生效。
func (a *API) handleUpdateConfig(ctx context.Context, c *app.RequestContext) {
	if a.config == nil {
		c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "config service unavailable"})
		return
	}
	var req service.ModelConfigUpdate
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if err := a.config.Update(ctx, req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, a.config.Get())
}

type taskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func (a *API) handleCreateTask(_ context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSession(context.Background(), c)
	if !ok {
		return
	}

	var req taskRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	now := a.now()
	t := &domain.Task{
		ID:          ids.New(),
		SessionID:   sessionID,
		Title:       req.Title,
		Description: req.Description,
		Enabled:     false, // 默认禁用，启用后才由 Runtime 创建 RunTask
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := a.tasks.Create(context.Background(), t); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, t)
}

func (a *API) handleListTasks(_ context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	tasks, err := a.tasks.ListBySession(context.Background(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"tasks": tasks})
}

// handleDeleteSession 删除 Session，并级联删除其名下的 Task、RunTask 与 ResearchState。
func (a *API) handleDeleteSession(ctx context.Context, c *app.RequestContext) {
	id, ok := a.requireSession(ctx, c)
	if !ok {
		return
	}

	tasks, err := a.tasks.ListBySession(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, t := range tasks {
		// 级联：先请求删除 RunTask（经 Runtime 协商后原子删除），再删 Task。
		if err := a.runs.RequestDeleteRunTask(ctx, t.ID); err != nil {
			c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := a.tasks.Delete(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	// 级联删除该 Session 的 ResearchState（研究发现资产随 Session 删除）。
	if err := a.runs.DeleteResearchState(ctx, id); err != nil && !errors.Is(err, researchstate.ErrNotFound) {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := a.sessions.Delete(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

type taskUpdateRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleUpdateTask 启用/禁用 Task。
// 状态变更经 RunService 以事件方式提交给 Runtime，由 Runtime 更新 RunTask。
func (a *API) handleUpdateTask(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "task id is required"})
		return
	}

	var req taskUpdateRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Enabled == nil {
		task, err := a.tasks.Get(ctx, id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				c.JSON(http.StatusNotFound, map[string]string{"error": "task not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, task)
		return
	}

	task, err := a.runs.SetTaskEnabled(ctx, id, *req.Enabled)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotFound):
			c.JSON(http.StatusNotFound, map[string]string{"error": "task not found"})
		default:
			c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, task)
}

// handleGetRunByTask 查询 Task 对应的 RunTask。
func (a *API) handleGetRunByTask(ctx context.Context, c *app.RequestContext) {
	taskID := c.Param("taskID")
	if taskID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "task id is required"})
		return
	}
	rt, err := a.runs.GetRunTaskByTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "run task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rt)
}

// handleGetRunTask 按 RunTaskID 查询。
func (a *API) handleGetRunTask(ctx context.Context, c *app.RequestContext) {
	runTaskID := c.Param("runTaskID")
	if runTaskID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "run task id is required"})
		return
	}
	rt, err := a.runs.GetRunTask(ctx, runTaskID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "run task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rt)
}

type messageRequest struct {
	Message string `json:"message"`
}

// handleSendMessage 向指定 Session 的研究提交一条用户消息（异步，由 Supervisor 处理）。
//
// 与本文件中的 handleResearchStream 前置校验一致，区别只在于响应方式：
// 该接口立即返回 202，不订阅事件流；需要实时输出时请使用 SSE 版本。
func (a *API) handleSendMessage(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSession(ctx, c)
	if !ok {
		return
	}

	var req messageRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if err := a.runs.SendUserMessage(ctx, sessionID, req.Message); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, map[string]string{"status": "accepted"})
}

// handleGetResearchState 查询指定 Session 的研究状态。
func (a *API) handleGetResearchState(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	st, err := a.runs.GetResearchState(ctx, sessionID)
	if err != nil {
		if errors.Is(err, researchstate.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "research state not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, st)
}

// handleGetHistory 返回指定 Session 的对话历史（前端切换会话时回放）。
func (a *API) handleGetHistory(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	msgs, err := a.runs.GetConversationHistory(ctx, sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"messages": msgs})
}

// handleStop 处理用户主动中止请求（协作式、全局范围）。
func (a *API) handleStop(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	if err := a.runs.RequestStop(ctx, sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]string{"status": "stopped"})
}

type approvalRequest struct {
	ApprovalID string `json:"approval_id"`
	Approved   bool   `json:"approved"`
	Reason     string `json:"reason"`
}

// handleApprove 处理工具调用的人工审批。
func (a *API) handleApprove(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	var req approvalRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ApprovalID) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "approval_id is required"})
		return
	}
	if err := a.runs.Approve(ctx, sessionID, req.ApprovalID, req.Approved, req.Reason); err != nil {
		c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]any{"approval_id": req.ApprovalID, "approved": req.Approved})
}

// handleResearchStream 以 SSE 流式返回 Supervisor 的研究输出。
// 借鉴 eino-examples a2ui 的事件消费方式：订阅流式输出事件，逐块写入 SSE。
func (a *API) handleResearchStream(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSession(ctx, c)
	if !ok {
		return
	}
	var req messageRequest
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	w := sse.NewWriter(c)
	sub := &streamSubscriber{sessionID: sessionID, w: w, done: make(chan struct{})}
	bus := a.runs.Bus()
	bus.Subscribe(event.EventModelOutput, sub)
	bus.Subscribe(event.EventModelOutputDone, sub)
	// 工具动作与审批请求同样经 SSE 推送到前端。
	bus.Subscribe(event.EventToolCall, sub)
	bus.Subscribe(event.EventToolResult, sub)
	bus.Subscribe(event.EventToolApprovalRequired, sub)
	bus.Subscribe(event.EventToolApprovalCancelled, sub)
	// 后台任务（Assignment / Work）的状态变化同样经 SSE 实时推送。
	bus.Subscribe(event.EventAssignmentUpdated, sub)
	bus.Subscribe(event.EventWorkCreated, sub)
	bus.Subscribe(event.EventWorkUpdated, sub)

	if err := a.runs.SendUserMessage(ctx, sessionID, req.Message); err != nil {
		_ = w.WriteEvent("", "error", []byte(err.Error()))
		_ = w.Close()
		return
	}

	select {
	case <-sub.done:
	case <-ctx.Done():
	case <-time.After(10 * time.Minute): // 含人工审批等待时间
	}
	_ = w.Close()
}

// handleSessionEvents 是**会话级**长连接 SSE：与任何一次研究请求无关，
// 只推送该 Session 的状态变化，供前端状态徽章与后台任务面板实时更新。
//
// 与 handleResearchStream 的分工：
//   - handleResearchStream：一次研究的输出流（message / tool_call / ...），
//     随本轮结束而关闭；
//   - handleSessionEvents ：常驻订阅（run_status / assignment_updated / work_*），
//     切换会话时才重建，因此研究在后台推进时徽章也能实时变化。
func (a *API) handleSessionEvents(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSession(ctx, c)
	if !ok {
		return
	}

	w := sse.NewWriter(c)
	sub := &sessionEventSubscriber{sessionID: sessionID, w: w}

	bus := a.runs.Bus()
	// RunTask 状态：状态徽章的唯一实时数据源。
	bus.Subscribe(event.EventRunStatusChanged, sub)
	// 后台任务：Assignment 调度状态与 Work 执行阶段。
	bus.Subscribe(event.EventAssignmentUpdated, sub)
	bus.Subscribe(event.EventWorkCreated, sub)
	bus.Subscribe(event.EventWorkUpdated, sub)
	defer func() {
		bus.Unsubscribe(event.EventRunStatusChanged, sub)
		bus.Unsubscribe(event.EventAssignmentUpdated, sub)
		bus.Unsubscribe(event.EventWorkCreated, sub)
		bus.Unsubscribe(event.EventWorkUpdated, sub)
		_ = w.Close()
	}()

	// 立即推送一次当前状态，避免前端等到下次变更才显示正确徽章。
	if rt, err := a.runs.CurrentRunTask(ctx, sessionID); err == nil && rt != nil {
		payload, mErr := json.Marshal(map[string]string{
			"task_id":     rt.TaskID,
			"run_task_id": rt.ID,
			"status":      string(rt.Status),
			"previous":    "",
		})
		if mErr == nil {
			_ = w.WriteEvent("", "run_status", payload)
		}
	}

	// 长连接：直到客户端断开或服务关闭。
	<-ctx.Done()
}

// sessionEventSubscriber 把会话级状态事件写入 SSE。
type sessionEventSubscriber struct {
	sessionID string
	w         *sse.Writer
}

var _ event.EventSender = (*sessionEventSubscriber)(nil)

// Send 实现 event.EventSender。
func (s *sessionEventSubscriber) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.RunStatusChangedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"task_id":     ev.TaskID,
			"run_task_id": ev.RunTaskID,
			"status":      string(ev.Status),
			"previous":    string(ev.Previous),
		})
		if err == nil {
			_ = s.w.WriteEvent("", "run_status", payload)
		}
	case event.WorkUpdatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(workUpdatePayload{
			WorkID:       ev.WorkID,
			AssignmentID: ev.AssignmentID,
			WorkerID:     ev.WorkerID,
			Goal:         ev.Goal,
			Status:       string(ev.Status),
			Stage:        ev.Stage,
			Detail:       ev.Detail,
			Attempt:      ev.Attempt,
			Err:          ev.Err,
			StartedAt:    ev.StartedAt,
			FinishedAt:   ev.FinishedAt,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "work_updated", payload)
		}
	case event.WorkCreatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"work_id":       ev.WorkID,
			"assignment_id": ev.AssignmentID,
			"goal":          ev.Goal,
			"status":        string(domain.WorkPending),
		})
		if err == nil {
			_ = s.w.WriteEvent("", "work_updated", payload)
		}
	case event.AssignmentUpdatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"assignment_id": ev.AssignmentID,
			"status":        string(ev.Status),
			"result":        ev.Result,
			"error":         ev.Err,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "assignment_updated", payload)
		}
	}
	return nil
}

// streamSubscriber 订阅流式输出与工具事件并写入 SSE。
type streamSubscriber struct {
	sessionID string
	w         *sse.Writer
	done      chan struct{}
	once      sync.Once
}

var _ event.EventSender = (*streamSubscriber)(nil)

// Send 实现 event.EventSender：过滤本 Session 的事件并写入 SSE。
//
// SSE 事件名：message（模型输出）/ tool_call / tool_result / approval_required。
func (s *streamSubscriber) Send(_ context.Context, e event.EventEnvelope) error {
	switch ev := e.Data.(type) {
	case event.ModelOutputEvent:
		if ev.SessionID == s.sessionID {
			_ = s.w.WriteEvent("", "message", []byte(ev.Chunk))
		}
	case event.ModelOutputDoneEvent:
		if ev.SessionID == s.sessionID {
			s.once.Do(func() { close(s.done) })
		}
	case event.ToolCallEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"tool_name": ev.ToolName,
			"arguments": ev.Arguments,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "tool_call", payload)
		}
	case event.ToolResultEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"tool_name": ev.ToolName,
			"output":    ev.Output,
			"error":     ev.Err,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "tool_result", payload)
		}
	case event.ToolApprovalRequiredEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"approval_id": ev.ApprovalID,
			"tool_name":   ev.ToolName,
			"arguments":   ev.Arguments,
			"description": ev.Description,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "approval_required", payload)
		}
	case event.ToolApprovalCancelledEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"approval_id": ev.ApprovalID,
			"reason":      ev.Reason,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "approval_cancelled", payload)
		}
	case event.WorkCreatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(workUpdatePayload{
			WorkID:       ev.WorkID,
			AssignmentID: ev.AssignmentID,
			WorkerID:     ev.WorkerID,
			Goal:         ev.Goal,
			Status:       string(domain.WorkPending),
			Stage:        "queued",
			Attempt:      1,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "work_updated", payload)
		}
	case event.WorkUpdatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(workUpdatePayload{
			WorkID:       ev.WorkID,
			AssignmentID: ev.AssignmentID,
			WorkerID:     ev.WorkerID,
			Goal:         ev.Goal,
			Status:       string(ev.Status),
			Stage:        ev.Stage,
			Detail:       ev.Detail,
			Attempt:      ev.Attempt,
			Err:          ev.Err,
			StartedAt:    ev.StartedAt,
			FinishedAt:   ev.FinishedAt,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "work_updated", payload)
		}
	case event.AssignmentUpdatedEvent:
		if ev.SessionID != s.sessionID {
			return nil
		}
		payload, err := json.Marshal(map[string]string{
			"assignment_id": ev.AssignmentID,
			"status":        string(ev.Status),
			"result":        ev.Result,
			"error":         ev.Err,
		})
		if err == nil {
			_ = s.w.WriteEvent("", "assignment_updated", payload)
		}
	}
	return nil
}

// workUpdatePayload 是推送给前端的后台任务（子 Agent）状态。
type workUpdatePayload struct {
	WorkID       string    `json:"work_id"`
	AssignmentID string    `json:"assignment_id"`
	WorkerID     string    `json:"worker_id,omitempty"`
	Goal         string    `json:"goal,omitempty"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
	Err          string    `json:"error,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
}

// backgroundTaskView 是后台任务面板的一行（Assignment 及其 Work 执行实例）。
type backgroundTaskView struct {
	AssignmentID string `json:"assignment_id"`
	Goal         string `json:"goal"`
	Status       string `json:"status"`
	Result       string `json:"result,omitempty"`
	Err          string `json:"error,omitempty"`

	WorkID     string    `json:"work_id,omitempty"`
	WorkerID   string    `json:"worker_id,omitempty"`
	WorkStatus string    `json:"work_status,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Attempt    int       `json:"attempt,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// handleBackgroundTasks 返回某个 Session 当前 RunTask 下的后台任务列表。
//
// 数据来源：AssignmentStore（调度状态） + WorkStore（执行实例与阶段）。
func (a *API) handleBackgroundTasks(ctx context.Context, c *app.RequestContext) {
	sessionID, ok := a.requireSessionID(c)
	if !ok {
		return
	}
	if a.assignments == nil || a.works == nil {
		c.JSON(http.StatusOK, map[string]any{"tasks": []backgroundTaskView{}})
		return
	}

	rt, err := a.runs.CurrentRunTask(ctx, sessionID)
	if err != nil || rt == nil {
		c.JSON(http.StatusOK, map[string]any{"tasks": []backgroundTaskView{}})
		return
	}

	assignments, err := a.assignments.ListByRunTask(ctx, rt.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	works, err := a.works.ListByRunTask(ctx, rt.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 每个 Assignment 取最新的 Work 作为展示对象。
	latestWork := make(map[string]*domain.Work, len(works))
	for _, w := range works {
		if w == nil {
			continue
		}
		if prev, ok := latestWork[w.AssignmentID]; !ok || w.CreatedAt.After(prev.CreatedAt) {
			latestWork[w.AssignmentID] = w
		}
	}

	tasks := make([]backgroundTaskView, 0, len(assignments))
	for _, as := range assignments {
		view := backgroundTaskView{
			AssignmentID: as.ID,
			Goal:         as.Goal,
			Status:       string(as.Status),
			Result:       as.Result,
			Err:          as.Err,
			Attempt:      as.Attempt,
		}
		if w, ok := latestWork[as.ID]; ok {
			view.WorkID = w.ID
			view.WorkerID = w.WorkerID
			view.WorkStatus = string(w.Status)
			view.Stage = w.Stage
			view.Detail = w.Detail
			view.Attempt = w.Attempt
			view.StartedAt = w.StartedAt
			view.FinishedAt = w.FinishedAt
			if view.Goal == "" {
				view.Goal = w.Goal
			}
		}
		tasks = append(tasks, view)
	}

	c.JSON(http.StatusOK, map[string]any{
		"run_task_id": rt.ID,
		"tasks":       tasks,
	})
}
