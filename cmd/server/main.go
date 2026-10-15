// Command server 是 DeepResearch Agent 的服务入口。
//
// 阶段 0：装配配置、日志、模型、存储与 HTTP 路由，启动 Hertz 服务。
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/api"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/config"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/service"
	"github.com/YinLanShan/go-deepresearch-agent.git/internal/storage/memory"
)

func main() {
	cfg := config.Load()
	setupLog(cfg.LogLevel)

	model, err := llm.New(context.Background(), llm.Config{
		Provider:  cfg.SupervisorModel.Provider,
		BaseURL:   cfg.SupervisorModel.BaseURL,
		APIKey:    cfg.SupervisorModel.APIKey,
		ModelName: cfg.SupervisorModel.ModelName,
	})
	if err != nil {
		slog.Error("init model failed", "err", err)
		os.Exit(1)
	}

	a := api.New(
		service.NewChatService(model),
		memory.NewSessionStore(),
		memory.NewTaskStore(),
	)

	h := server.Default(
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithSenseClientDisconnection(true),
	)
	a.Register(h)

	slog.Info("server starting", "addr", cfg.HTTPAddr, "model_provider", cfg.SupervisorModel.Provider)
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
