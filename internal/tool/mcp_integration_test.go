package tool

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/tool/mcptestserver"
)

// buildTestServer 把 cmd/mcptestserver 编译成临时可执行文件，供 stdio 传输使用。
func buildTestServer(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "mcptestserver")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/mcptestserver")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mcptestserver: %v\n%s", err, out)
	}
	return bin
}

// toolNames 提取工具名集合。
func toolNames(t *testing.T, tools []tool.BaseTool) map[string]tool.BaseTool {
	t.Helper()
	out := make(map[string]tool.BaseTool, len(tools))
	for _, tl := range tools {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		out[info.Name] = tl
	}
	return out
}

// invoke 调用一个可执行工具并返回文本结果。
func invoke(t *testing.T, tl tool.BaseTool, args string) (string, error) {
	t.Helper()
	inv, ok := tl.(tool.InvokableTool)
	if !ok {
		t.Fatalf("tool %T is not invokable", tl)
	}
	return inv.InvokableRun(context.Background(), args)
}

// TestLoadMCPStdio 用真实子进程 + stdio 传输连接 MCP Server，
// 验证工具发现与多次调用的完整往返。
func TestLoadMCPStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP stdio test in short mode")
	}

	bin := buildTestServer(t)

	res, err := LoadMCP(context.Background(), MCPServerConfig{
		Name:      "testserver",
		Transport: "stdio",
		Command:   bin,
	})
	if err != nil {
		t.Fatalf("LoadMCP stdio: %v", err)
	}
	// 关闭会话以终止 stdio 子进程（否则 Windows 下临时目录无法删除）。
	t.Cleanup(func() { _ = res.Close() })

	byName := toolNames(t, res.Tools)
	for _, want := range []string{"echo", "add", "fail_tool", "slow_task"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("tool %q not discovered, got %v", want, keys(byName))
		}
	}

	// 真实调用：echo
	out, err := invoke(t, byName["echo"], `{"text":"hello-mcp"}`)
	if err != nil {
		t.Fatalf("invoke echo: %v", err)
	}
	if !strings.Contains(out, "hello-mcp") {
		t.Fatalf("echo output = %q, want contains hello-mcp", out)
	}

	// 真实调用：add（结构化入参）
	out, err = invoke(t, byName["add"], `{"a":17,"b":25}`)
	if err != nil {
		t.Fatalf("invoke add: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("add output = %q, want 42", out)
	}
}

// TestLoadMCPToolError 验证工具自身的错误能作为错误返回（供错误兜底中间件处理）。
func TestLoadMCPToolError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP error test in short mode")
	}

	bin := buildTestServer(t)
	res, err := LoadMCP(context.Background(), MCPServerConfig{
		Name:      "testserver",
		Transport: "stdio",
		Command:   bin,
		ToolNameList: []string{
			"fail_tool",
		},
	})
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	if len(res.Tools) != 1 {
		t.Fatalf("ToolNameList should filter to 1 tool, got %d", len(res.Tools))
	}

	_, err = invoke(t, res.Tools[0], `{"reason":"boom"}`)
	if err == nil {
		t.Fatal("expected error from fail_tool")
	}
	if !strings.Contains(err.Error(), "intentional failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadMCPStreamableHTTP 通过真实 HTTP（Streamable HTTP）连接 MCP Server。
func TestLoadMCPStreamableHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP http test in short mode")
	}

	srv := httptest.NewServer(mcptestserver.Handler())
	defer srv.Close()

	res, err := LoadMCP(context.Background(), MCPServerConfig{
		Name:      "testserver-http",
		Transport: "http",
		URL:       srv.URL,
	})
	if err != nil {
		t.Fatalf("LoadMCP http: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	byName := toolNames(t, res.Tools)
	if _, ok := byName["echo"]; !ok {
		t.Fatalf("echo not discovered over http, got %v", keys(byName))
	}

	out, err := invoke(t, byName["echo"], `{"text":"over-http"}`)
	if err != nil {
		t.Fatalf("invoke echo over http: %v", err)
	}
	if !strings.Contains(out, "over-http") {
		t.Fatalf("echo output = %q", out)
	}
}

// TestRegistryWithRealMCP 验证工具注册表能从真实 MCP Server 装配出工具，
// 并且审批策略按名字生效。
func TestRegistryWithRealMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MCP registry test in short mode")
	}

	bin := buildTestServer(t)
	reg := NewRegistry(context.Background(), RegistryConfig{
		MCPServers: []MCPServerConfig{{
			Name:      "testserver",
			Transport: "stdio",
			Command:   bin,
		}},
		EnableBuiltinSearch: false,
		RequireApproval:     []string{"echo"},
	})

	if reg.Len() == 0 {
		t.Fatal("registry should load tools from real MCP server")
	}
	t.Cleanup(func() { _ = reg.Close() })
	if !reg.Policy().NeedsApproval("echo") {
		t.Fatal("echo should require approval per policy")
	}
	if reg.Policy().NeedsApproval("add") {
		t.Fatal("add should not require approval")
	}
}

// keys 返回 map 的键（仅用于测试诊断）。
func keys(m map[string]tool.BaseTool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
