# Eino TurnLoop 抢占与流式输出（可复用结论）

- 依赖版本：`github.com/cloudwego/eino v0.9.21`（`eino/adk`）。
- 源码：`eino/adk/turn_loop.go`、`eino/adk/interface.go`、`eino/schema/stream.go`。
- 官方示例：`eino-examples/quickstart/chatwitheino/a2ui/streamer.go`（事件消费与流式写入 SSE 的范式）。

## 已确认的调用流程

1. **TurnLoop 抢占**：`loop.Push(item, adk.WithPreempt[Input,*schema.Message](adk.AnySafePoint))`
   - `SafePoint` 取值 `AfterChatModel` / `AfterToolCalls` / `AnySafePoint`（两者位或）。
   - 抢占在模型调用/工具调用边界取消当前轮次；`runAgentAndHandleEvents` 中
     `watchPreempt` 调用 `agentCancelFunc(...)` 提交取消信号，`TurnContext.Preempted`
    （即内部 `preemptDone` 通道）在取消生效时关闭。

2. **抢占检测**：在 `OnAgentEvents(ctx, tc, events)` 内对 `tc.Preempted` 做非阻塞 select：
   ```go
   select { case <-tc.Preempted: /* 已抢占 */ default: }
   ```

3. **流式输出消费**：`genInput` 返回 `&adk.AgentInput{Messages, EnableStreaming: true}` 时，
   Agent 事件走流式：`event.Output.MessageOutput` 为 `TypedMessageVariant`，`IsStreaming=true`，
   `Message=nil`，实际内容在 `MessageStream`（`*schema.StreamReader[*schema.Message]`），
   需逐块 `Recv()` 读取；`io.EOF` 结束，取消/抢占时 `Recv()` 返回非 EOF 错误。

4. **自定义流式模型**：`schema.Pipe[T](cap)` 返回 `(*StreamReader[T], *StreamWriter[T])`，
   发送方 goroutine 内 `sw.Send(v, err)` / `defer sw.Close()`，`Send` 返回 `closed bool`。

## 已验证的使用方式

- 慢速流式模型（`internal/llm/mock.SlowModel`）在 `Stream` 中用 `Pipe` 逐块发送并
  在每块间 `select { <-time.After; <-ctx.Done() }` 响应取消，用于触发可观察的抢占。
- 抢占发生时，`OnAgentEvents` 已读取到的 chunk 即"被中断的上下文原文"，可累积保存。

## 尚未确认的限制

- `OnAgentEvents` 返回后到 `tc.Preempted` 关闭的精确时序未逐一验证；本项目在事件循环
  结束前后都做一次非阻塞检查来规避竞态。
- 流式 chunk 按 rune 切分（避免把多字节 UTF-8 字符切断），非 Eino 约束而是本项目
  对 SSE 客户端的兼容性要求。
