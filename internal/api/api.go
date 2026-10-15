// Package api 是 API Layer，基于 Hertz 提供 HTTP 接口与 SSE 事件输出。
//
// 对应 docs/Architecture.md 6.2 节 API Layer 的定位：负责请求校验与事件输出，
// 不参与研究决策。阶段 0 提供：
//   - POST /api/v1/chat           —— 调用模型并通过 SSE 流式返回结果
//   - POST /api/v1/sessions       —— 创建 Session
//   - POST /api/v1/sessions/:id/tasks —— 在 Session 下创建 Task
//   - GET  /api/v1/sessions/:id/tasks —— 列出 Session 下的 Task
//   - DELETE /api/v1/sessions/:id      —— 删除 Session（级联删除其 Task）
//   - DELETE /api/v1/sessions/:id/tasks/:taskID —— 删除 Session 下的 Task
//
// 约束：一个 Session 只对应一个 Task，重复创建返回 409。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/sse"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/domain"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage"
)

// API 聚合各层依赖，暴露 HTTP 处理器。
type API struct {
	chat     *service.ChatService
	sessions storage.SessionStore
	tasks    storage.TaskStore
	now      func() time.Time
}

// New 构造一个 API 实例。
func New(chat *service.ChatService, sessions storage.SessionStore, tasks storage.TaskStore) *API {
	return &API{chat: chat, sessions: sessions, tasks: tasks, now: time.Now}
}

// Register 在路由上注册全部 HTTP 接口。
// 参数使用 route.IRouter，兼容生产环境 *server.Hertz 与测试环境 *route.Engine。
func (a *API) Register(r route.IRouter) {
	v1 := r.Group("/api/v1")
	v1.POST("/chat", a.handleChat)
	v1.POST("/sessions", a.handleCreateSession)
	v1.POST("/sessions/:id/tasks", a.handleCreateTask)
	v1.GET("/sessions/:id/tasks", a.handleListTasks)
	v1.DELETE("/sessions/:id", a.handleDeleteSession)
	v1.DELETE("/sessions/:id/tasks/:taskID", a.handleDeleteTask)
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
	sess := &domain.Session{ID: newID(), Title: req.Title, CreatedAt: now, UpdatedAt: now}
	if err := a.sessions.Create(context.Background(), sess); err != nil {
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sess)
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
		ID:          newID(),
		SessionID:   sessionID,
		Title:       req.Title,
		Description: req.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := a.tasks.Create(context.Background(), t); err != nil {
		if errors.Is(err, storage.ErrSessionHasTask) {
			c.JSON(http.StatusConflict, map[string]string{"error": "session already has a task"})
			return
		}
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

// handleDeleteSession 删除 Session，并级联删除其名下的 Task（1:1，最多一个）。
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
		if err := a.tasks.Delete(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
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

// handleDeleteTask 删除指定 Session 下的 Task。
func (a *API) handleDeleteTask(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	taskID := c.Param("taskID")
	if sessionID == "" || taskID == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "session id and task id are required"})
		return
	}

	t, err := a.tasks.Get(ctx, taskID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if t.SessionID != sessionID {
		c.JSON(http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	if err := a.tasks.Delete(ctx, taskID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// newID 生成随机 ID。crypto/rand 失败时回退到纳秒时间戳。
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
