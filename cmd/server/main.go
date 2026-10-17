// Command server 是 DeepResearch Agent 的服务入口。
//
// 阶段 0：装配配置、日志、模型、存储与 HTTP 路由，启动 Hertz 服务。
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/api"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/config"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/event"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/runtime"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

func main() {
	cfg := config.Load()
	setupLog(cfg.LogLevel)

	model, err := llm.New(context.Background(), llm.Config{
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
	bus := event.NewBus()

	// 注册模拟依赖组件（Supervisor / Scheduler / WorkPool 的占位）。
	// Runtime 发起生命周期协商时，由它们回执"同意"；后续替换为真实实现。
	runtime.RegisterMockCoordinators(bus, "supervisor", "scheduler", "workpool")

	// Runtime 是维护 RunTask 生命周期的独立实体，单独注册并启动。
	rt := runtime.New(runTaskStore, bus)
	rt.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rt.Shutdown(ctx)
	}()

	a := api.New(
		service.NewChatService(model),
		memory.NewSessionStore(),
		taskStore,
		service.NewRunService(taskStore, runTaskStore, bus),
	)

	h := server.Default(
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithSenseClientDisconnection(true),
	)
	a.Register(h)

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
