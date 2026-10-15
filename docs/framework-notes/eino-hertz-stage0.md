# 阶段 0 框架核查结论（Eino / Hertz）

> 按 AGENTS.md「框架 API 核查流程」记录的可复用结论，供后续阶段参考。
> 所有结论均以当前依赖版本源码 / 官方示例 / 实际运行结果为依据。

## 依赖版本

| 模块 | 版本 | 来源 |
| --- | --- | --- |
| github.com/cloudwego/eino | v0.9.21 | 本地 `../eino`，HEAD = `v0.9.21-1-g58d18430` |
| github.com/cloudwego/hertz | v0.10.6 | 本地 `../hertz`，`version.go` 声明 `v0.10.6` |
| github.com/cloudwego/eino-ext/components/model/openai | v0.1.13 | 模块缓存 |

## 已确认的 API 与调用流程

### 1. Eino 模型调用（流式）

- 接口 `model.BaseChatModel = model.BaseModel[*schema.Message]`，核心方法：
  - `Generate(ctx, []*schema.Message, ...model.Option) (*schema.Message, error)`
  - `Stream(ctx, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error)`
- 流式读取：`StreamReader.Recv()` 逐块返回，读到末尾返回 `io.EOF`；调用方必须 `Close()`。
- 消息构造：`schema.SystemMessage(s)` / `schema.UserMessage(s)` / `schema.AssistantMessage(s, nil)`。
- 测试用内存流：`schema.StreamReaderFromArray[T](arr)`。
- 源码依据：`eino/components/model/interface.go`、`eino/schema/stream.go`、`eino/schema/message.go`。

### 2. OpenAI 兼容模型构造

- `openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey, BaseURL, Model})` → `*openai.ChatModel`。
- 仅构造客户端，不做网络请求、不校验密钥；可指向任意 OpenAI 兼容服务（OpenAI / DeepSeek 等）。
- `*openai.ChatModel` 实现 `model.ToolCallingChatModel`（含 `WithTools`），其 `Stream` 签名与项目内 `llm.ChatModel` 完全一致，可直接复用。
- 源码依据：`eino-ext/components/model/openai@v0.1.13/chatmodel.go`；示例 `eino-examples/quickstart/chat/openai.go`。

### 3. Hertz SSE

- `sse.NewWriter(c *app.RequestContext)` 自动设置 `Content-Type: text/event-stream; charset=utf-8` 与 `Cache-Control: no-cache`，并 hijack 响应写入 chunked body。
- `w.WriteEvent(id, eventType string, data []byte) error`；`w.Close() error`。
- 客户端断开通过 handler 的 `ctx.Done()` 感知，需 `server.WithSenseClientDisconnection(true)`。
- 源码依据：`hertz/pkg/protocol/sse/writer.go`；示例 `hertz-examples/sse/server/main.go`。

### 4. Hertz 路由与测试

- 生产：`server.Default(server.WithHostPorts(":8080"))` + `h.Spin()`。
- 路由抽象 `route.IRouter`（含 `Group` + `GET/POST/...`）：`*server.Hertz`（内嵌 `*route.Engine`）与 `*route.Engine` 均实现，`Register(route.IRouter)` 可统一生产与测试。
- 非 SSE 测试：`route.NewEngine(config.NewOptions([]config.Option{}))` + `ut.PerformRequest(...)`。
- SSE 测试必须用真实连接：`server.WithListener(ln)` 绑定随机端口 + `net/http` 客户端读取。

## 已确认的限制

1. `ut.PerformRequest` 的内存环境没有网络 writer，SSE（hijack/chunked writer）会触发 nil 指针，**不能用 ut 直接测 SSE**；必须走真实 HTTP 连接。
2. openai 模型构造离线可行，但真实 `Stream` 需要密钥与服务端网络，本阶段未做线上调用验证（mock 已覆盖全链路）。
3. sonic 在 Go 1.27 环境回退到 `encoding/json`（官方警告），不影响功能正确性。
