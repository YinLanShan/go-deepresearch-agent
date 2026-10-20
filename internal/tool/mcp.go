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

// connectTimeout 是连接 MCP Server 的超时时间。
const connectTimeout = 20 * time.Second

// LoadMCP 连接一个 MCP Server 并返回其工具列表。
//
// 复用官方 go-sdk 客户端与 eino-ext 的 officialmcp 组件，不自行实现 MCP 协议。
func LoadMCP(ctx context.Context, cfg MCPServerConfig) ([]tool.BaseTool, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "deepresearch-agent",
		Version: "0.1.0",
	}, nil)

	var transport mcp.Transport
	switch cfg.Transport {
	case "http", "streamable-http", "":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp server %q: url is required for http transport", cfg.Name)
		}
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL}
	case "stdio":
		if cfg.Command == "" {
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
		return nil, fmt.Errorf("mcp server %q: unsupported transport %q", cfg.Name, cfg.Transport)
	}

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: connect failed: %w", cfg.Name, err)
	}

	tools, err := officialmcp.GetTools(connectCtx, &officialmcp.Config{
		Cli:          session,
		ToolNameList: cfg.ToolNameList,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: list tools failed: %w", cfg.Name, err)
	}
	return tools, nil
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
