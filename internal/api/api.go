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
}

// New 构造一个 API 实例。
func New(chat *service.ChatService, sessions storage.SessionStore, tasks storage.TaskStore,
	runs *service.RunService, cfg *service.ConfigService) *API {
	return &API{chat: chat, sessions: sessions, tasks: tasks, runs: runs, config: cfg, now: time.Now}
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
	v1.POST("/sessions/:id/stop", a.handleStop)
	v1.POST("/sessions/:id/research", a.handleResearchStream)
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
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if _, err := a.sessions.Get(context.Background(), sessionID); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
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
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
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
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if _, err := a.sessions.Get(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
func (a *API) handleSendMessage(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if _, err := a.sessions.Get(ctx, sessionID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
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

// handleStop 处理用户主动中止。
func (a *API) handleStop(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if err := a.runs.RequestStop(ctx, sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, map[string]string{"status": "stopped"})
}

// handleResearchStream 以 SSE 流式返回 Supervisor 的研究输出。
// 借鉴 eino-examples a2ui 的事件消费方式：订阅流式输出事件，逐块写入 SSE。
func (a *API) handleResearchStream(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id is required"})
		return
	}
	if _, err := a.sessions.Get(ctx, sessionID); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
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

	if err := a.runs.SendUserMessage(ctx, sessionID, req.Message); err != nil {
		_ = w.WriteEvent("", "error", []byte(err.Error()))
		_ = w.Close()
		return
	}

	select {
	case <-sub.done:
	case <-ctx.Done():
	case <-time.After(60 * time.Second):
	}
	_ = w.Close()
}

// streamSubscriber 订阅流式输出事件并写入 SSE。
type streamSubscriber struct {
	sessionID string
	w         *sse.Writer
	done      chan struct{}
	once      sync.Once
}

var _ event.EventSender = (*streamSubscriber)(nil)

// Send 实现 event.EventSender：过滤本 Session 的流式输出事件。
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
	}
	return nil
}
