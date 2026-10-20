// Command server 是 DeepResearch Agent 的服务入口。
//
// 阶段 0：装配配置、日志、模型、存储与 HTTP 路由，启动 Hertz 服务。
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/api"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/checkpoint"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/config"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
)

func main() {
	cfg := config.Load()
	setupLog(cfg.LogLevel)

	// 构建可热切换的模型：API 配置界面保存后立即生效，无需重启服务。
	model, err := llm.NewSwappable(context.Background(), llm.Config{
		Provider:  cfg.Model.Provider,
		BaseURL:   cfg.Model.BaseURL,
		APIKey:    cfg.Model.APIKey,
		ModelName: cfg.Model.ModelName,
	})
	if err != nil {
		slog.Error("init model failed", "err", err)
		os.Exit(1)
	}

	taskStore := memory.NewTaskStore()
	runTaskStore := memory.NewRunTaskStore()
	stateStore := stateMemory.NewStore()
	bus := event.NewBus()

	// 注册模拟依赖组件（Supervisor / Scheduler / WorkPool 的占位）。
	// Runtime 发起生命周期协商时，由它们回执"同意"；后续替换为真实实现。
	runtime.RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")

	// 装配工具：MCP Server（可选）+ 内置演示搜索工具，以及中断用 Checkpoint 存储。
	toolRegistry, checkPointStore := setupTools(cfg)

	// Runtime 是维护 RunTask 生命周期的独立实体，单独注册并启动。
	// 它在 RunTask 启动时初始化 ResearchState 与 SupervisorLoop。
	rt := runtime.New(runTaskStore, taskStore, stateStore, model, bus)
	rt.SetTools(toolRegistry, checkPointStore)
	rt.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Shutdown(ctx)
	}()

	runService := service.NewRunService(taskStore, runTaskStore, stateStore, bus)
	// 审批决定交给 Runtime → SupervisorLoop，由 Eino 的 Resume 流程恢复执行。
	runService.SetApprover(rt)

	a := api.New(
		service.NewChatService(model),
		memory.NewSessionStore(),
		taskStore,
		runService,
		service.NewConfigService(model),
	)

	h := server.Default(
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithSenseClientDisconnection(true),
	)
	// CORS：允许前端模板（web/index.html 经 file:// 打开）跨源访问本服务。
	h.Use(corsMiddleware)
	a.Register(h)

	// 同时把前端模板挂在根路径，便于直接访问 http://<addr>/ 使用。
	h.StaticFile("/", "./web/index.html")

	slog.Info("server starting", "addr", cfg.HTTPAddr, "model_provider", cfg.Model.Provider)
	h.Spin()
}

func setupLog(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

// setupTools 装配 Supervisor 可用的工具与中断用 Checkpoint 存储。
//
// MCP Server 通过 DRA_MCP_SERVERS 配置（JSON 数组）；未配置时退回内置演示搜索工具，
// 以便在没有外部 MCP Server 的情况下也能验证 ReAct 工具调用、审批与前端展示链路。
//
// 说明：审批不再使用阻塞等待，而是由中间件调用 tool.StatefulInterrupt 暂停本轮，
// ADK 负责把执行状态序列化进 Checkpoint；用户决定后经 Resume 恢复。
func setupTools(cfg config.Config) (*agenttool.Registry, adk.CheckPointStore) {
	var servers []agenttool.MCPServerConfig
	if strings.TrimSpace(cfg.Tool.MCPServersJSON) != "" {
		if err := json.Unmarshal([]byte(cfg.Tool.MCPServersJSON), &servers); err != nil {
			slog.Error("parse DRA_MCP_SERVERS failed", "err", err)
		}
	}

	var requireApproval []string
	for _, p := range strings.Split(cfg.Tool.RequireApproval, ",") {
		if p = strings.TrimSpace(p); p != "" {
			requireApproval = append(requireApproval, p)
		}
	}

	reg := agenttool.NewRegistry(context.Background(), agenttool.RegistryConfig{
		MCPServers:          servers,
		EnableBuiltinSearch: cfg.Tool.EnableBuiltinSearch,
		RequireApproval:     requireApproval,
	})

	slog.Info("tools ready", "count", reg.Len(), "names", reg.Names(),
		"require_approval", requireApproval, "mcp_servers", len(servers))
	return reg, checkpoint.NewMemoryStore()
}

// corsMiddleware 为跨源请求设置 CORS 头，并放行预检请求。
func corsMiddleware(ctx context.Context, c *app.RequestContext) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Content-Type")
	if string(c.Method()) == "OPTIONS" {
		c.AbortWithStatus(204)
		return
	}
	c.Next(ctx)
}
