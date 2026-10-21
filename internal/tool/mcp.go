package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	officialmcp "github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// MCPServerConfig 描述一个 MCP Server 的连接方式。
type MCPServerConfig struct {
	// Name 仅用于日志与错误信息。
	Name string `json:"name"`

	// Transport 支持 "stdio"（本地子进程）与 "http"（Streamable HTTP）。
	Transport string `json:"transport"`

	// Command/Args/Env 用于 stdio 传输。
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`

	// URL 用于 http 传输。
	URL string `json:"url"`

	// ToolNameList 指定只加载哪些工具；为空表示加载全部。
	ToolNameList []string `json:"tool_name_list"`
}

// MCPTools 是连接 MCP Server 的结果，持有会话以便释放（关闭子进程 / 断开 HTTP 长连接）。
type MCPTools struct {
	// Tools 是从该 Server 加载的工具。
	Tools []tool.BaseTool

	session *mcp.ClientSession
	// cancel 结束会话生命周期上下文。
	cancel context.CancelFunc
}

// Close 关闭 MCP 会话；对 stdio 传输会终止其子进程，
// 对 Streamable HTTP 会断开长连接（hanging GET）并停止重连。
func (m *MCPTools) Close() error {
	if m == nil {
		return nil
	}
	var err error
	if m.session != nil {
		err = m.session.Close()
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	return err
}

// connectTimeout 是 MCP 握手（initialize + tools/list）的超时时间。
const connectTimeout = 20 * time.Second

// LoadMCP 连接一个 MCP Server 并返回其工具列表。
//
// 复用官方 go-sdk 客户端与 eino-ext 的 officialmcp 组件，不自行实现 MCP 协议。
// 调用方应在不再需要工具时调用返回值的 Close 释放会话。
//
// 重要：会话必须绑定到"比 LoadMCP 更长的上下文"。
// Streamable HTTP 传输会保持一条 hanging GET 长连接用于接收服务端通知，
// 它继承 Connect 时传入的 ctx；若该 ctx 随 LoadMCP 返回而 cancel，
// 长连接会断开，后续 tools/call 将报
// "client is closing ... failed to reconnect ... context canceled"。
// 因此这里用 context.WithoutCancel 派生会话上下文，仅在 Close 时取消。
func LoadMCP(ctx context.Context, cfg MCPServerConfig) (*MCPTools, error) {
	// 会话级上下文：不随调用方 ctx 取消，由 Close 负责结束。
	sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "deepresearch-agent",
		Version: "0.1.0",
	}, nil)

	var transport mcp.Transport
	switch cfg.Transport {
	case "http", "streamable-http", "":
		if cfg.URL == "" {
			cancelSession()
			return nil, fmt.Errorf("mcp server %q: url is required for http transport", cfg.Name)
		}
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL}
	case "stdio":
		if cfg.Command == "" {
			cancelSession()
			return nil, fmt.Errorf("mcp server %q: command is required for stdio transport", cfg.Name)
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		if len(cfg.Env) > 0 {
			env := os.Environ()
			for k, v := range cfg.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		transport = &mcp.CommandTransport{Command: cmd}
	default:
		cancelSession()
		return nil, fmt.Errorf("mcp server %q: unsupported transport %q", cfg.Name, cfg.Transport)
	}

	// 握手阶段单独限时：超时后取消会话上下文，避免长时间挂起。
	handshakeTimer := time.AfterFunc(connectTimeout, cancelSession)
	defer handshakeTimer.Stop()

	session, err := client.Connect(sessionCtx, transport, nil)
	if err != nil {
		cancelSession()
		return nil, fmt.Errorf("mcp server %q: connect failed: %w", cfg.Name, err)
	}

	tools, err := officialmcp.GetTools(sessionCtx, &officialmcp.Config{
		Cli:          session,
		ToolNameList: cfg.ToolNameList,
	})
	if err != nil {
		_ = session.Close()
		cancelSession()
		return nil, fmt.Errorf("mcp server %q: list tools failed: %w", cfg.Name, err)
	}
	return &MCPTools{Tools: tools, session: session, cancel: cancelSession}, nil
}

// ─── 内置工具（无 MCP Server 时的可运行兜底） ────────────────────────────────

// WebSearchInput 是 web_search 工具入参。
type WebSearchInput struct {
	Query string `json:"query" jsonschema:"description=搜索关键词"`
}

// WebSearchOutput 是 web_search 工具出参。
type WebSearchOutput struct {
	Query   string   `json:"query"`
	Results []string `json:"results"`
	Note    string   `json:"note"`
}

// NewBuiltinWebSearch 构造一个内置的演示搜索工具。
//
// 它不访问外网，只返回确定性结果，用于在未配置 MCP Server 时验证
// ReAct 工具调用、审批与前端展示链路。接入真实搜索请配置 MCP Server。
func NewBuiltinWebSearch() (tool.InvokableTool, error) {
	return utils.InferTool[WebSearchInput, WebSearchOutput](
		"web_search",
		"搜索互联网资料。输入搜索关键词，返回若干条摘要结果。用于调研类问题。",
		func(_ context.Context, in WebSearchInput) (WebSearchOutput, error) {
			if in.Query == "" {
				return WebSearchOutput{}, fmt.Errorf("query is required")
			}
			return WebSearchOutput{
				Query: in.Query,
				Results: []string{
					"[1] " + in.Query + " — 概述与背景（内置演示数据）",
					"[2] " + in.Query + " — 架构与实现细节（内置演示数据）",
					"[3] " + in.Query + " — 性能对比与社区反馈（内置演示数据）",
				},
				Note: "builtin demo search; configure a MCP server for real search",
			}, nil
		},
	)
}
