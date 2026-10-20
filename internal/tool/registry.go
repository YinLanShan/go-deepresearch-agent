package tool

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/components/tool"
)

// Policy 决定哪些工具需要人工审批。
type Policy struct {
	// RequireApproval 是工具名列表；"*" 表示全部。
	RequireApproval []string
}

// NewPolicy 构造审批策略，自动去空白并忽略空项。
func NewPolicy(names []string) Policy {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return Policy{RequireApproval: out}
}

// NeedsApproval 判断某个工具是否需要审批。
func (p Policy) NeedsApproval(name string) bool {
	for _, candidate := range p.RequireApproval {
		if candidate == "*" || strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

// RegistryConfig 描述工具注册表如何装配。
type RegistryConfig struct {
	// MCPServers 是要连接的 MCP Server 列表。
	MCPServers []MCPServerConfig
	// EnableBuiltinSearch 为 true 时注册内置演示搜索工具（无 MCP 也能跑通链路）。
	EnableBuiltinSearch bool
	// RequireApproval 指定需要人工审批的工具名；支持 "*" 表示全部。
	RequireApproval []string
}

// Registry 持有从 MCP / 内置来源加载的原始工具，可被多个 SupervisorLoop 复用。
//
// 审批、错误兜底与前端展示都由中间件实现（见 middleware.go），
// 因此这里只负责"加载工具"，不对工具做任何包装。
type Registry struct {
	raw    []tool.BaseTool
	policy Policy

	// sessions 持有已连接的 MCP 会话，用于释放（关闭 stdio 子进程）。
	sessions []*MCPTools
}

// NewRegistry 按配置装配工具。单个 MCP Server 连接失败只记录告警，不阻断启动。
func NewRegistry(ctx context.Context, cfg RegistryConfig) *Registry {
	r := &Registry{policy: NewPolicy(cfg.RequireApproval)}

	for _, server := range cfg.MCPServers {
		res, err := LoadMCP(ctx, server)
		if err != nil {
			slog.Warn("load mcp tools failed", "server", server.Name, "err", err)
			continue
		}
		slog.Info("mcp tools loaded", "server", server.Name, "count", len(res.Tools))
		r.raw = append(r.raw, res.Tools...)
		r.sessions = append(r.sessions, res)
	}

	if cfg.EnableBuiltinSearch {
		if search, err := NewBuiltinWebSearch(); err != nil {
			slog.Warn("register builtin web_search failed", "err", err)
		} else {
			r.raw = append(r.raw, search)
		}
	}
	return r
}

// Close 释放全部 MCP 会话（关闭 stdio 子进程等）。
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var firstErr error
	for _, s := range r.sessions {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.sessions = nil
	return firstErr
}

// Tools 返回原始工具列表（未包装），供注入 ToolsNodeConfig。
func (r *Registry) Tools() []tool.BaseTool {
	if r == nil {
		return nil
	}
	return r.raw
}

// Policy 返回审批策略。
func (r *Registry) Policy() Policy {
	if r == nil {
		return Policy{}
	}
	return r.policy
}

// Len 返回已注册的工具数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.raw)
}

// Names 返回已注册的工具名（用于诊断与前端展示）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.raw))
	for _, t := range r.raw {
		if info, err := t.Info(context.Background()); err == nil {
			out = append(out, info.Name)
		}
	}
	return out
}
