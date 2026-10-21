# Eino 工具治理：中间件 + 中断恢复（MCP）

- 依赖版本：`eino v0.9.21`、`eino-ext/components/tool/mcp/officialmcp v0.1.0`、
  `modelcontextprotocol/go-sdk v1.0.0`。
- 官方参考：`eino-examples/quickstart/chatwitheino/cmd/ch05`（中间件）、
  `cmd/ch10`（HTTP + 审批 + ResumeWithParams）、`helpers/middleware.go`（工具错误兜底）。

## 1. 三条治理能力都用中间件实现

```go
adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
    ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
    Handlers: []adk.ChatModelAgentMiddleware{
        approvalMiddleware, // 审批：最外层
        safeToolMiddleware, // 工具报错兜底
        eventMiddleware,    // 工具动作上报（前端展示）：最内层
    },
})
```

- 包裹顺序即注册顺序，第一个注册的是最外层。
- 三者都实现 `WrapInvokableToolCall(ctx, endpoint, *adk.ToolContext)`。

### 1.1 审批中间件（不阻塞）

```go
wasInterrupted, _, storedArgs := tool.GetInterruptState[string](ctx)
if !wasInterrupted {
    return "", tool.StatefulInterrupt(ctx, ApprovalInfo{...}, args)
}
isTarget, hasData, data := tool.GetResumeContext[ApprovalResult](ctx)
if isTarget && hasData && data.Approved {
    return endpoint(ctx, storedArgs, opts...)
}
```

- 首次调用 → `tool.StatefulInterrupt` 暂停整轮，ADK 负责序列化执行状态。
- 恢复时从 resume 上下文取用户决定，批准才真正执行。

### 1.2 工具错误兜底

把普通错误转成 `"[tool error] ..."` 文本回传模型，避免单个工具失败中断整轮；
**但 `compose.IsInterruptRerunError` 必须原样抛出**，否则审批恢复链路被破坏。

### 1.3 动作上报

在 `endpoint` 前后上报 `ToolStarted/ToolFinished`，经事件总线以 SSE 推送到前端。

## 2. 中断恢复（Checkpoint）关键约束（已踩坑）

1. **gob 注册**：中断载荷作为 `any` 进入 Checkpoint，必须注册，否则
   `failed to encode checkpoint: gob: type not registered for interface`：

   ```go
   schema.RegisterName[ApprovalInfo]("dra_tool_approval_info")
   schema.RegisterName[ApprovalResult]("dra_tool_approval_result")
   ```

   **gob 同一类型只能注册一个名字**，因此统一使用值类型（不要指针/值各注册一次，
   否则 `registering duplicate names` panic）。

2. **Runner 必须拿到 CheckpointID**：没有 ID 时 Runner 不落盘，
   TurnLoop 的 Checkpoint 会出现 `HasRunnerState=false`，
   于是恢复时走 `GenInput`（而不是 `GenResume`），表现为"审批后毫无反应"。

3. **TurnLoop 中断后 Run 返回**：`Run()` 内部 `runOnce`，重复调用是空操作。
   恢复必须**新建 TurnLoop 实例**（同一 `CheckpointID` + `Store`）再 `Run()`，
   由 `tryLoadCheckpoint` 发现 runner 状态并调用 `GenResume`。

4. **`GenResume` 只负责构造 `ResumeParams`**：

   ```go
   &adk.GenResumeResult[Input, *schema.Message]{
       ResumeParams: &adk.ResumeParams{Targets: map[string]any{interruptID: ApprovalResult{...}}},
       Consumed:     interruptedItems,
   }
   ```

5. **中断本轮不要提交业务结果**：中断时 `OnAgentEvents` 应返回 nil 且跳过 Commit，
   也不发布"输出完成"，以便前端 SSE 保持打开等待审批。

6. **CheckPointStore 接口**仅 `Get(ctx,id) ([]byte,bool,error)` / `Set(ctx,id,[]byte)error`，
   eino 未提供内存实现；项目实现于 `internal/checkpoint`（另加 `Delete` 以满足
   `CheckPointDeleter`）。这里只做字节存取，**序列化与恢复语义完全由 ADK 负责**。

## 3. 真实 MCP 链路（已实测）

服务端用官方 `modelcontextprotocol/go-sdk` 实现（`internal/tool/mcptestserver`），
客户端用 `officialmcp.GetTools`，两端都是真实协议，非 mock。

### 3.1 服务端最小写法（官方 API）

```go
s := mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0.1.0"}, nil)
mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "..."},
    func(ctx context.Context, req *mcp.CallToolRequest, in Args) (*mcp.CallToolResult, any, error) {
        return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "..."}}}, nil, nil
    })
s.Run(ctx, &mcp.StdioTransport{})                                   // stdio
mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil) // http
```

- 参数/返回结构由 `json` + `jsonschema` tag 自动生成 schema。
- **stdio 模式禁止向 stdout 打印任何内容**，否则污染协议流。

### 3.2 客户端要点

- `LoadMCP` 返回 `*MCPTools`，内部持有 `*mcp.ClientSession`；
  必须调用 `Close()`，否则 stdio 子进程不退出（Windows 下临时目录无法删除）。
- 工具调用结果形如 `{"content":[{"type":"text","text":"..."}],"isError":false}`，
  由 `officialmcp` 序列化为 JSON 字符串返回。
- 工具自身报错时返回 error，**错误信息里已包含服务端返回体**。

### 3.2.1 会话上下文不能随 Connect 返回而取消（重要）

`StreamableClientTransport` 会保持一条 **hanging GET 长连接**用于接收服务端通知，
它继承 `client.Connect(ctx, ...)` 传入的 ctx。

错误写法：

```go
connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
defer cancel()                              // ← LoadMCP 返回即取消
session, _ := client.Connect(connectCtx, transport, nil)
```

后果：长连接被杀，之后任何 `tools/call` 都会失败并触发重连，而重连用的 ctx 已取消：

```
failed to call official mcp tool: connection closed: calling "tools/call":
client is closing: hanging GET: failed to reconnect (session ID: ...):
connection failed after 5 attempts: ... context canceled
```

stdio 不受影响（无 hanging GET），所以该问题只在 HTTP 传输下暴露。

正确写法：会话绑定到"比函数更长"的上下文，仅由 `Close` 取消，握手超时单独控制。

```go
sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))
timer := time.AfterFunc(connectTimeout, cancelSession) // 仅覆盖握手阶段
defer timer.Stop()
session, _ := client.Connect(sessionCtx, transport, nil)
// ... Close() 中调用 cancelSession()
```

### 3.2.2 测试清理顺序（httptest 会挂死）

`httptest.Server.Close()` 会等待所有在途请求，其中包括那条 hanging GET。
若用 `defer srv.Close()`，它在函数返回时执行，早于 `t.Cleanup(res.Close)`，
于是**永远等不到长连接结束，测试挂死**。

正确做法：让会话先于服务器关闭（`t.Cleanup` 后进先出，先注册 `srv.Close`）：

```go
srv := httptest.NewServer(...)
t.Cleanup(srv.Close)                     // 先注册 → 最后执行
res, _ := LoadMCP(...)
t.Cleanup(func() { _ = res.Close() })    // 后注册 → 先执行
```

### 3.3 已实测结论

| 场景 | 结果 |
|---|---|
| stdio 子进程连接 | 发现 4 个工具，`echo`/`add` 调用成功 |
| Streamable HTTP 连接 | 同上 |
| HTTP 空闲 22s+ 后再调用 | 正常（会话上下文修复后，见 3.2.1） |
| `ToolNameList` 过滤 | 只加载指定工具 |
| 工具报错 | `SafeToolMiddleware` 转为 `[tool error] ...`，ReAct 继续产出结论 |
| 审批 + 恢复 | `message → approval_required →（批准）→ tool_call → tool_result → message` |

## 4. 尚未确认

- 中断与 Stop/抢占并发时的优先级细节。
- Checkpoint 的持久化介质（当前为进程内，多实例部署需换 Redis/DB 实现同一接口）。
- MCP 的 OAuth / 鉴权传输（`oauthex`）未接入。

