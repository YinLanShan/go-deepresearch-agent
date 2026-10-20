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

## 3. 已验证结论

- 中断后 Checkpoint 约 25KB，`HasRunnerState=true`，`CanceledItems=1`（被中断的输入项）。
- `Approve` → 新 TurnLoop `Run()` → `GenResume` → 工具真正执行 → 提交 Finding。
- HTTP 全链路：`message → approval_required →（批准）→ tool_call → tool_result → message`。

## 4. 尚未确认

- 中断与 Stop/抢占并发时的优先级细节。
- Checkpoint 的持久化介质（当前为进程内，多实例部署需换 Redis/DB 实现同一接口）。
