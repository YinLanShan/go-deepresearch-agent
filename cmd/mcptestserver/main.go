// Command mcptestserver 是一个独立可运行的 MCP 测试服务器。
//
// 用法：
//
//	mcptestserver -transport stdio                      # 供客户端以子进程拉起
//	mcptestserver -transport http -addr 127.0.0.1:PORT  # 独立 HTTP 服务
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/tool/mcptestserver"
)

func main() {
	transport := flag.String("transport", "stdio", "传输方式：stdio 或 http")
	addr := flag.String("addr", "127.0.0.1:18080", "http 传输的监听地址")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transport {
	case "stdio":
		// 注意：stdio 模式不要向 stdout 打印任何内容（会污染 MCP 协议流）。
		if err := mcptestserver.RunStdio(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("mcp test server failed: %v", err)
		}
	case "http":
		srv := &http.Server{
			Addr:    *addr,
			Handler: mcptestserver.Handler(),
		}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		log.Printf("mcp test server listening on http://%s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("mcp test server failed: %v", err)
		}
	default:
		log.Fatalf("unsupported transport %q", *transport)
	}
}
