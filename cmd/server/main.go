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
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/fault"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
	stateMemory "github.com/YinLanShan/go-deepresearch-agent.git/internal/researchstate/memory"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/resultagg"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/scheduler"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
	agenttool "github.com/YinLanShan/go-deepresearch-agent.git/internal/tool"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/workpool"
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
	assignmentStore := memory.NewAssignmentStore()
	workStore := memory.NewWorkStore()
	bus := event.NewBus()

	// 装配工具：MCP Server（可选）+ 内置演示搜索工具，以及中断用 Checkpoint 存储。
	toolRegistry, checkPointStore := setupTools(cfg)
	// 退出时释放 MCP 会话（关闭 stdio 子进程）。
	defer func() { _ = toolRegistry.Close() }()

	// ─── 受 Runtime 管理的三个独立执行实体 ───────────────────────────────
	// 依赖方向：Scheduler → WorkPool / ResultAggregator → ResearchState。
	// ResultAggregator 通过 SchedulerNotifier 回调 Scheduler 完成回收确认。
	// 它们都参与 Runtime 的协商握手（订阅 CoordinationRequested）。
	//
	// 两级并发控制：
	//   MaxConcurrency（Scheduler）：同时在途的 Work 上限（排队 + 执行），提供背压；
	//   Workers（WorkPool）：真正的并行执行槽位。
	// MaxConcurrency < Workers 时部分 Worker 会闲置，属于配置错误。
	if cfg.Execution.MaxConcurrency < cfg.Execution.Workers {
		slog.Warn("max_concurrency is less than workers, some workers will stay idle",
			"max_concurrency", cfg.Execution.MaxConcurrency,
			"workers", cfg.Execution.Workers,
			"hint", "set DRA_SCHEDULER_MAX_CONCURRENCY >= DRA_WORKPOOL_WORKERS")
	}
	aggregator := resultagg.New(resultagg.Config{
		States: stateStore,
		Bus:    bus,
	})
	// Worker 用工厂：每个 Work 创建独立的子 Agent（与 Supervisor 共用模型）。
	// 子 Agent 只拿到业务工具，控制类工具（submit/cancel/finish）被过滤掉。
	// 错误上报器：写日志 + 按 Severity 决定是否推送前端（Info 只写日志）。
	reporter := fault.NewReporter(fault.ReporterConfig{
		Bus:   bus,
		Debug: getenvBool("DRA_DEBUG_ERRORS", false),
	})
	workPool := workpool.New(workpool.Config{
		Works: workStore,
		Bus:   bus,
		Factory: workpool.NewSubAgentFactory(workpool.SubAgentConfig{
			Model: model,
			Tools: toolRegistry.Tools(),
		}),
		Workers:     cfg.Execution.Workers,
		WorkTimeout: cfg.Execution.WorkTimeout,
		Reporter:    reporter,
	})
	sched := scheduler.New(scheduler.Config{
		Assignments:    assignmentStore,
		Works:          workStore,
		Pool:           workPool,
		Slots:          aggregator,
		Bus:            bus,
		MaxConcurrency: cfg.Execution.MaxConcurrency,
		Reporter:       reporter,
	})
	// 提交确认回传给 Scheduler（Commit 成功后 Scheduler 才回收 Assignment）。
	aggregator.SetSchedulerNotifier(sched)

	// Runtime 是维护 RunTask 生命周期的独立实体，单独注册并启动。
	// 它在 RunTask 启动时初始化 ResearchState 与 SupervisorLoop。
	rt := runtime.New(runTaskStore, taskStore, stateStore, model, bus)
	rt.SetTools(toolRegistry, checkPointStore)
	rt.SetExecution(assignmentStore, workStore, sched, workPool, aggregator)
	// 复用同一个上报器：保证同类错误在全局范围内合并计数。
	rt.SetReporter(reporter)
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
	// 后台任务（Assignment/Work）查询接口的数据来源。
	a.SetExecutionStores(assignmentStore, workStore)

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
// getenvBool 读取布尔环境变量（"1"/"true"/"yes" 视为真）。
func getenvBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func setupTools(cfg config.Config) (*agenttool.Registry, adk.CheckPointStore) {
	var servers []agenttool.MCPServerConfig
	if strings.TrimSpace(cfg.Tool.MCPServersJSON) != "" {
		if err := json.Unmarshal([]byte(cfg.Tool.MCPServersJSON), &servers); err != nil {
			slog.Error("parse DRA_MCP_SERVERS failed", "err", err)
		}
	}

	// 审批策略：支持 "-" / "none" 显式关闭；留空沿用默认（全部审批）。
	requireApproval := cfg.Tool.ApprovalRequired()

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
