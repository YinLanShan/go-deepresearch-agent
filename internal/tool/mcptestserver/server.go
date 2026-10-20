// Package mcptestserver 提供一个真实可运行的 MCP Server，用于端到端测试。
//
// 它使用官方 modelcontextprotocol/go-sdk 实现服务端（本项目只做客户端接入），
// 支持两种传输：
//   - stdio：由客户端以子进程方式拉起（DRA_MCP_SERVERS 配置 command）；
//   - http ：独立监听端口，客户端以 Streamable HTTP 连接。
//
// 提供的工具：
//   - echo      回显文本（验证基本收发）
//   - add       两数相加（验证结构化入参）
//   - fail_tool 始终返回错误（验证工具错误兜底中间件）
//   - slow_task 延迟返回（验证中断/抢占时机）
package mcptestserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName 是注册到 MCP 的服务器标识。
const ServerName = "deepresearch-mcp-testserver"

// New 构造一个注册了全部测试工具的 MCP Server。
func New() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: "0.1.0",
	}, nil)

	// echo：回显文本。
	type echoArgs struct {
		Text string `json:"text" jsonschema:"要回显的文本"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "回显输入的文本，用于验证 MCP 链路连通性。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	// add：两数相加。
	type addArgs struct {
		A int `json:"a" jsonschema:"第一个加数"`
		B int `json:"b" jsonschema:"第二个加数"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "add",
		Description: "计算两个整数之和。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in addArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d", in.A+in.B)}},
		}, nil, nil
	})

	// fail_tool：始终报错，用于验证工具错误兜底。
	type failArgs struct {
		Reason string `json:"reason" jsonschema:"失败原因"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "fail_tool",
		Description: "总是返回错误，用于验证工具错误处理。",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in failArgs) (*mcp.CallToolResult, any, error) {
		return nil, nil, fmt.Errorf("fail_tool intentional failure: %s", in.Reason)
	})

	// slow_task：延迟返回，用于验证中断/抢占时机。
	type slowArgs struct {
		Milliseconds int `json:"milliseconds" jsonschema:"延迟毫秒数"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "slow_task",
		Description: "延迟指定毫秒后返回，用于验证中断时机。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in slowArgs) (*mcp.CallToolResult, any, error) {
		d := time.Duration(in.Milliseconds) * time.Millisecond
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("slept %s", d)}},
		}, nil, nil
	})

	return s
}

// RunStdio 以 stdio 传输运行服务器（供子进程方式使用）。
func RunStdio(ctx context.Context) error {
	return New().Run(ctx, &mcp.StdioTransport{})
}

// Handler 返回 Streamable HTTP 的 http.Handler，用于进程内或独立 HTTP 服务。
func Handler() *mcp.StreamableHTTPHandler {
	s := New()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
