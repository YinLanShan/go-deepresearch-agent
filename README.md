# DeepResearch Agent

基于 Go + Eino + CloudWeGo Hertz 的可扩展 DeepResearch Agent Runtime。

项目规划见 `docs/`，当前进度：**阶段 0（工程骨架 + 接口打通）**。

## 阶段 0 能力

- Hertz HTTP 服务
- Eino 流式模型调用（OpenAI 兼容接口 / 内置 mock）
- SSE 流式输出
- Session / Task 数据结构与存储接口（内存实现）

## 目录结构

```
cmd/server/          服务入口
internal/config/     环境变量配置
internal/fault/      统一错误包装
internal/domain/     Session / Task 领域对象
internal/storage/    存储接口 + 内存实现
internal/llm/        模型调用封装（openai / mock）
internal/service/    Application Layer 业务编排
internal/api/        API Layer（Hertz 路由 + SSE）
```

## 运行

```bash
# 默认 mock 模型，无需密钥，端口 :8080
go run ./cmd/server

# 使用 OpenAI 兼容模型（可指向 DeepSeek 等）
$env:DRA_MODEL_PROVIDER="openai"
$env:DRA_OPENAI_API_KEY="sk-..."
$env:DRA_OPENAI_BASE_URL="https://api.deepseek.com/v1"
$env:DRA_OPENAI_MODEL="deepseek-chat"
go run ./cmd/server
```

### 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DRA_HTTP_ADDR` | `:8080` | HTTP 监听地址 |
| `DRA_LOG_LEVEL` | `info` | debug/info/warn/error |
| `DRA_MODEL_PROVIDER` | `mock` | `openai` 或 `mock` |
| `DRA_OPENAI_BASE_URL` | 空 | OpenAI 兼容接口地址 |
| `DRA_OPENAI_API_KEY` | 空 | 模型鉴权密钥 |
| `DRA_OPENAI_MODEL` | `gpt-4o-mini` | 模型名称 |

### 接口

> Windows 注意：`curl.exe -d '{"...":"..."}'` 的引号会被 PowerShell/cmd 吞掉，
> 中文也可能被按本地代码页编码，导致服务端收到非法 JSON 而返回
> `invalid request body`。推荐用下面的 PowerShell 方式（原生处理 JSON 与 UTF-8），
> 或改用 JSON 文件方式。

```powershell
# 调用模型并流式返回（SSE）
Invoke-WebRequest -Uri http://127.0.0.1:8080/api/v1/chat ` -Method Post -ContentType "application/json" -Body '{"message":"你好"}'

# 创建 Session
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions ` -Method Post -ContentType "application/json" -Body '{"title":"研究主题"}'

# 在 Session 下创建 Task
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/tasks ` -Method Post -ContentType "application/json" -Body '{"title":"子任务","description":"..."}'

# 列出 Session 下的 Task
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/tasks

# 删除 Session（会级联删除其名下的 Task 与 RunTask）
Invoke-WebRequest -Uri http://127.0.0.1:8080/api/v1/sessions/{id} -Method Delete

# 启用/禁用 Task（经事件驱动 Runtime 创建/暂停 RunTask）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/tasks/{id} `
  -Method Patch -ContentType "application/json" -Body '{"enabled":true}'

# 查询 Task 对应的 RunTask
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/tasks/{taskID}/run

# 按 RunTaskID 查询
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/run-tasks/{runTaskID}

# 向 Session 的研究提交一条用户消息（异步，由 Supervisor 处理）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/messages `
  -Method Post -ContentType "application/json" -Body '{"message":"请分析架构"}'

# 查询 Session 的研究状态（研究需求 + 各轮研究结论）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/research-state

# 流式研究：提交消息并以 SSE 流式返回 Supervisor 的研究输出
curl.exe -s -N -X POST http://127.0.0.1:8080/api/v1/sessions/{id}/research `
  -H "Content-Type: application/json" --data-binary "@$env:TEMP\msg.json"

# 用户主动中止（协作式/全局；中止后 RunTask 置 stopped，ResearchState 停止接收 Commit）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/stop `
  -Method Post -ContentType "application/json" -Body '{}'

# 列出全部 Session（前端侧边栏使用）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions

# 读取当前模型配置（密钥脱敏）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/config

# 运行时热切换模型配置（立即生效，无需重启）
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/config -Method Put `
  -ContentType "application/json" `
  -Body '{"provider":"openai","base_url":"https://api.deepseek.com","model_name":"deepseek-chat","api_key":"sk-..."}'

# 工具调用审批：批准/拒绝某次待审批的工具调用
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/approve `
  -Method Post -ContentType "application/json" `
  -Body '{"approval_id":"<来自 SSE 的 approval_required>","approved":true}'
```

> 约束：
> - **一个 Session 可对应多个 Task**；一个 Task 对应一个 RunTask。
> - **同一时间全局只有一个 RunTask 处于 Running**；被挤掉的 RunTask 置 `waiting`
>   （不是 `paused`），Task 置不可用，Checkpoint 与待审批项保留，可随时"继续"切回。
> - **Task 只能启用/禁用，无直接删除路径**；新建的 Task 默认禁用。
> - **RunTask 由独立实体 Runtime 经事件订阅维护**：启用/禁用/删除都先由 Runtime
>   向依赖组件（Supervisor/Scheduler/WorkPool，当前为 Mock）发起协商，协商通过后才原子落地。
> - **使能时若 RunTask 不存在，Runtime 先协商创建（再新建 RunTask**，
>   同时初始化该 Task 的 ResearchState 并启动 SupervisorLoop；该 Session 下
>   所有已结束节点以完整快照写入 `RelatedNodes`（用于上下文展开）。
> - **失能 = 用户主动暂停 + 状态置 `paused`**：停止 SupervisorLoop、关闭提交门控；
>   再次使能（"继续"）由 `paused` 置回 `running` 并重新打开门控。
> - **用户主动中止 = 状态置 `stopped`（终态）**：作废待审批项（发布 `approval_cancelled`）、
>   删除 Checkpoint、停止 SupervisorLoop；`InterruptedContext` 保留为该节点的历史记录；
>   不关闭提交门控，属于该 Task 的迟到 Result 仍可提交。"继续"不会复活。
> - **模型完成研究 = 状态置 `finished`**：终止态。此后用户再发消息，会在该 Session 下
>   自动新建 Task 与新 ResearchState（前序节点完整继承）并继续研究。
> - **SupervisorLoop** 外层订阅用户消息事件（按 RunTaskID 路由），内层用 Eino 的
>   TurnLoop 驱动模型产出研究结论，写回该 Task 的 ResearchState；流式输出经
>   ModelOutput 事件发布为 SSE。
> - **ResearchState Commit 门控**：暂停/等待时停止接收新 Commit（返回 ErrNotAccepting），
>   正在进行的 Commit 继续完成。
> - **模型可热切换**：`main` 持有 `llm.Swappable`，`PUT /api/v1/config` 只替换内部模型实例，
>   Runtime / SupervisorLoop / ChatService 无需重建。
> - ResearchState 归属 Task；Session 级联删除时一并删除其下全部 ResearchState。

## ResearchState 与上下文构建

每个 Task 一份 ResearchState，是该节点研究资产（证据 + 知识）的权威来源：

| 字段 | 语义 | 是否被 Commit 消费 |
|---|---|---|
| `requirement` | 用户首条消息即研究需求 | 否 |
| `results` | 研究产物（证据），含 pending 状态 | 追加 |
| `findings` | 研究结论（知识） | 追加 |
| `interrupted_context` | 中断时累积的推理原文 | **否，永不消费** |
| `related_nodes` | 前序节点的**完整快照**（需求/Result/Finding/中断原文） | 否 |

`BuildContext()` 按节点分区构建，前序节点完整展开而非摘要：

```
# 前序研究节点（已完成或已中止的历史，按时间顺序）
## 第 1 个研究节点
【研究需求】… 【研究产物 Result（第 1 个节点的证据）】…
【研究结论 Finding（第 1 个节点的知识）】…
【被中断的上下文原文（第 1 个节点的中断记录）】…

# 当前研究节点（第 2 个）
【研究需求】… 【研究产物 Result（第 2 个节点的证据）】…
【研究结论 Finding（第 2 个节点的知识）】…
【被中断的上下文原文（第 2 个节点的中断记录）】…

# 当前任务与决策要求
```

说明：
- `Commit` 只追加 Result + Finding，**不触碰 `interrupted_context`**；
- 任务中止后到达的迟到产物走 `AppendResult`，**只追加 Result**，不产生 Finding。


## 工具（MCP）与审批

Supervisor 是 ReAct Agent。工具在 `prepareAgent` 时注入 `ChatModelAgentConfig.ToolsConfig`，
由 ChatModelAgent 完成「推理 → 工具调用 → 观察 → 再推理」循环。

**工具来源**（复用开源实现，不自行实现 MCP 协议）：

| 配置 | 说明 |
|---|---|
| `DRA_MCP_SERVERS` | MCP Server 列表的 JSON 数组；空则只用内置工具 |
| `DRA_ENABLE_BUILTIN_SEARCH` | 默认 `true`，注册内置演示 `web_search`（返回确定性数据，便于无 MCP 时验证链路） |
| `DRA_TOOL_REQUIRE_APPROVAL` | 需审批的工具名，逗号分隔；`*` 表示全部（默认） |

`DRA_MCP_SERVERS` 示例：

```json
[
  {"name":"fs","transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","D:\\data"]},
  {"name":"remote","transport":"http","url":"http://127.0.0.1:3001/mcp","tool_name_list":["search"]}
]
```

> HTTP 传输注意：MCP 会话必须绑定到长生命周期上下文（项目用
> `context.WithoutCancel` + `Close` 取消）。若让握手超时的 ctx 在 `LoadMCP` 返回时被取消，
> Streamable HTTP 的 hanging GET 会断开，后续调用报
> `client is closing ... failed to reconnect ... context canceled`。详见
> [docs/framework-notes/eino-mcp-tool-approval.md](docs/framework-notes/eino-mcp-tool-approval.md) 3.2.1。

### 本地真实 MCP 测试

仓库自带一个可运行的 MCP Server（`cmd/mcptestserver`，基于官方 Go SDK 实现服务端），
提供 `echo` / `add` / `fail_tool` / `slow_task` 四个工具，用于无需第三方依赖的真实链路验证。

```powershell
go build -o bin\mcptestserver.exe ./cmd/mcptestserver

# 方式一：HTTP（独立进程）
.\bin\mcptestserver.exe -transport http -addr 127.0.0.1:18080
$env:DRA_MCP_SERVERS = '[{"name":"test","transport":"http","url":"http://127.0.0.1:18080"}]'

# 方式二：stdio（由 Agent 拉起子进程，更接近实际 MCP 部署）
$env:DRA_MCP_SERVERS = '[{"name":"test","transport":"stdio","command":"D:\\path\\bin\\mcptestserver.exe"}]'

$env:DRA_MODEL_PROVIDER   = "mock-tool"   # 确定性模型，会调用模型实际可见的工具
$env:DRA_ENABLE_BUILTIN_SEARCH = "false"  # 只用真实 MCP 工具
$env:DRA_TOOL_REQUIRE_APPROVAL = "*"
.\bin\server.exe
```

启动日志应出现 `mcp tools loaded server=test server=... count=4` 与
`tools ready count=4 names="[add echo fail_tool slow_task]"`。

自动化的真实 MCP 集成测试（会编译并拉起真实服务器子进程）：

```powershell
go test -count=1 -v ./internal/tool/          # stdio + http + 错误路径 + 注册表
go test -count=1 ./... -short                 # 跳过 MCP 集成测试，仅跑快测
```

**治理方式：全部用中间件注入**（参考 `chatwitheino/cmd/ch05`、`ch10`）：

| 中间件 | 职责 |
|---|---|
| `ApprovalMiddleware` | 需审批的工具在首次调用时 `tool.StatefulInterrupt` 中断本轮 |
| `SafeToolMiddleware` | 工具错误转成 `[tool error] ...` 文本回传模型；中断重入错误原样抛出 |
| `EventMiddleware` | 调用前后上报 `ToolStarted/ToolFinished`，经 SSE 推给前端 |

三者通过 `ChatModelAgentConfig.Handlers` 注入，按「审批 → 错误兜底 → 动作上报」顺序包裹。

**审批流程（Eino 中断 + Checkpoint 恢复，无阻塞）**：

1. 工具命中审批策略 → 中间件返回 `tool.StatefulInterrupt`，本轮暂停。
2. ADK 把执行状态（含 ReAct 图与 runner 状态）序列化进 Checkpoint；
   TurnLoop 以 `Store + CheckpointID + GenResume` 配置保存/加载。
3. SSE 推送 `approval_required`，前端渲染审批卡片。
4. 用户 `POST /sessions/:id/approve` → Runtime 找到该 Session 的 SupervisorLoop →
   用同一 CheckpointID **新建 TurnLoop 并 Run**，Eino 的 `GenResume` 构造 `ResumeParams`，
   ADK 从"工具调用前"的状态恢复执行。
5. 批准 → 工具真正执行；拒绝 → 返回说明文本，ReAct 继续推理。
6. Finding 只取「最后一段 assistant 文本」，中间推理仅用于展示。

> 中断载荷类型必须在 gob 中注册（`schema.RegisterName`），否则 Checkpoint 序列化失败。
> 详见 [docs/framework-notes/eino-mcp-tool-approval.md](docs/framework-notes/eino-mcp-tool-approval.md)。

## 前端控制台

`web/index.html` 是单文件前端（无构建步骤），布局参考 eino-examples 的 chatwitheino：

- 顶栏：`⚙ API 配置`、`+ 新建研究`
- 左侧：研究会话列表（可删除），切换会话会加载该会话历史对话
- 中间：卡片式聊天区，Supervisor 输出以 SSE 流式增量渲染（Markdown、自动滚动）；
  工具动作渲染为等宽紧凑卡片（工具调用 / 工具结果），审批请求渲染为「需要审批」卡片
- 右侧：ResearchState 面板（需求 / 节点序号 / 版本 / Finding / Result / 中断上下文 / 前序节点）
- 头部：RunTask 状态徽章（研究中 / 已暂停 / **等待中** / 已中止 / 已完成），每 800ms 轮询刷新
- 底部：输入框 + 主按钮 + 中止。主按钮随状态切换：
  - `running` → 发送
  - `paused` / `waiting` → **继续**（切回运行，会把其他会话挤成 `waiting`）
  - `finished` / `stopped` → 发送（发送后自动新建 Task 与新 ResearchState）

交互细节：
- 只有真正产生文本时才创建 Supervisor 卡片；工具调用后模型若无输出，不会留下空白卡片
  （因此也不会出现孤立的光标与角色标签）。
- 「API 配置」弹窗点击遮罩关闭时，要求按下与松开都发生在遮罩上；
  在弹窗内选中文本拖到弹窗外不会误关闭。

使用方式（二选一）：

1. **直接由服务提供**：启动服务后访问 `http://127.0.0.1:8080/`。
2. **以文件方式打开**：浏览器打开 `web/index.html`（`file://`），
   在「API 配置 → 服务地址」填写 `http://127.0.0.1:8080`（服务端已开启 CORS）。

「API 配置」支持设置服务地址、模型提供方（mock / mock-tool / openai）、Base URL、
模型名称与 API Key；保存后立即生效。密钥留空表示沿用已保存的密钥，界面只展示脱敏提示。

**配置不全不会导致模型不可用**：

| 字段 | 留空时的行为 |
|---|---|
| Base URL | 回退默认 `https://api.deepseek.com` |
| 模型名称 | 回退默认 `deepseek-chat` |
| 提供方未变 | 沿用当前已保存的 Base URL / 模型名（只改密钥不会清空自定义配置） |
| 提供方切换 | 使用新提供方的默认值，避免沿用旧模型名导致接口拒绝 |

用 curl.exe 时，把 JSON 写入文件（无 BOM 的 UTF-8）再发送，可避免引号/编码问题：

```powershell
[System.IO.File]::WriteAllText("$env:TEMP\req.json", '{"title":"研究主题"}', [System.Text.UTF8Encoding]::new($false))
curl.exe -s -X POST http://127.0.0.1:8080/api/v1/sessions `
  -H "Content-Type: application/json" --data-binary "@$env:TEMP\req.json"
```

## 验证

```bash
go build ./... && go vet ./... && go test ./...
```
