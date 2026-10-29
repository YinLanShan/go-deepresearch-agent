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
internal/fault/      统一错误包装 + 错误上报
internal/domain/     Session / Task / RunTask / Assignment / Work 领域对象
internal/storage/    存储接口 + 内存实现
internal/researchstate/  ResearchState 与提交门控
internal/supervisor/ SupervisorLoop（Eino TurnLoop + 工具治理）
internal/scheduler/  Assignment 调度
internal/workpool/   Work 执行（子 Agent）
internal/resultagg/  结果聚合与提交
internal/tool/       工具注册、审批与安全中间件
internal/llm/        模型调用封装（openai / mock）+ 重试
internal/service/    Application Layer 业务编排
internal/api/        API Layer（Hertz 路由 + SSE）
docs/                架构、错误手册、数据模型
```

**相关文档**：

- [docs/Architecture.md](docs/Architecture.md) — 总体架构
- [docs/ErrorHandbook.md](docs/ErrorHandbook.md) — 错误类型与处理清单
- [docs/DataModel.md](docs/DataModel.md) — PostgreSQL 持久化数据模型

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

# 启用/禁用 Task（启用=创建/恢复 RunTask；禁用=等价于中止，终态）
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
> - **同一时间全局只有一个 RunTask 处于 Running**；被挤掉的 RunTask 置 `waiting`，
>   Task 置不可用，Checkpoint 与待审批项保留，可随时"继续"切回。
> - **Task 只能启用/禁用，无直接删除路径**；新建的 Task 默认禁用。
> - **RunTask 由独立实体 Runtime 经事件订阅维护**：启用/禁用/删除都先由 Runtime
>   向依赖组件（Supervisor/Scheduler/WorkPool）发起协商，协商通过后才原子落地。
> - **使能时若 RunTask 不存在，Runtime 先协商创建（再新建 RunTask**，
>   同时初始化该 Task 的 ResearchState 并启动 SupervisorLoop；该 Session 下
>   所有已结束节点以完整快照写入 `RelatedNodes`（用于上下文展开）。
> - **失能 = 用户中止 + 状态置 `stopped`（终态）**：停止 SupervisorLoop、作废待审批项、
>   删除 Checkpoint；再次使能不能复活，用户继续研究会在新 Task 上进行。
> - **不存在"暂停后继续"**：用户只有"中止"这一个选择，中止即终态。
> - **用户主动中止 = 状态置 `stopped`（终态）**：作废待审批项（发布 `approval_cancelled`）、
>   删除 Checkpoint、停止 SupervisorLoop；`InterruptedContext` 保留为该节点的历史记录；
>   不关闭提交门控，属于该 Task 的迟到 Result 仍可提交。"继续"不会复活。
> - **模型主动结束研究 = 状态置 `finished`（终态）**：唯一入口是模型调用
>   `finish_research` 工具。调用后不可撤销，且无论是否仍有 Assignment 在运行或
>   等待提交，一律取消（走 ActionStop 协商）。此后用户再发消息，会在该 Session 下
>   自动新建 Task 与新 ResearchState（前序节点完整继承）并继续研究。
> - **"本轮结束" ≠ "研究结束"**：模型派发 Assignment 后即可结束本轮（RunTask 仍为
>   `running`，SupervisorLoop 保留），不阻塞等待子任务；结果提交后由
>   `ResearchStateCommitted` 唤醒下一轮。本轮内用户再发消息仍属同一 Task。
> - **两种唤醒方式**：
>   - 运行中的轮次收到结果提交 → **抢占**（中断当前轮次，被打断输出并入下一轮 Finding）；
>   - 轮次**空闲**时若子任务**全部终结**（成功/失败/撤销皆算）→ 由
>     `AllAssignmentsSettled` 唤醒一次，让模型评估是否给出结论。
>     同一个空闲期只唤醒一次，避免撤销/失败连续触发时反复唤醒；
>     轮次运行中则保持原有抢占机制，不额外入队。
> - **用户消息优先级最高**：始终带抢占发送，可打断正在进行的轮次；
>   被打断的输出进入 `InterruptedContext`，下一轮并入 Finding（结果延迟提交）。
> - **SupervisorLoop** 外层订阅用户消息事件（按 RunTaskID 路由），内层用 Eino 的
>   TurnLoop 驱动模型产出研究结论，写回该 Task 的 ResearchState；流式输出经
>   ModelOutput 事件发布为 SSE。
> - **ResearchState Commit 门控**：等待（被挤掉）时停止接收新 Commit（返回 ErrNotAccepting），
>   正在进行的 Commit 继续完成。
> - **模型可热切换**：`main` 持有 `llm.Swappable`，`PUT /api/v1/config` 只替换内部模型实例，
>   Runtime / SupervisorLoop / ChatService 无需重建。
> - ResearchState 归属 Task；Session 级联删除时一并删除其下全部 ResearchState。

## 执行平面：Scheduler / WorkPool / ResultAggregator

三者都是**受 Runtime 管理的独立生命周期实体**，各自拥有启动与停止流程，
不参与 SupervisorLoop 的推理过程。边界遵循 `docs/Architecture.md` 5.6：

```
Supervisor 决定研究什么  →  Scheduler 决定什么时候执行  →  Work 实际执行
```

| 实体 | 职责（本阶段实现） | 暂不实现 |
|---|---|---|
| `Scheduler` | Assignment 排队、并发控制、Worker 分配、全局中止时状态标记 | 优先级、依赖检查、超时、Retry、Resource Limit |
| `WorkPool` | 固定数量 Worker、Work 执行、阶段上报（子 Agent 实时进度） | 超时中断、真实 SubAgent、工具调用 |
| `ResultAggregator` | 槽位预留、校验、批量 Commit、通知 Scheduler 与 Supervisor | 去重、冲突处理、FindingGraph、上下文压缩 |

**协作协议**（对应 7.3.8 / 7.6.4）：

1. Scheduler 创建 Assignment 调度记录时，先向 ResultAggregator **预留结果槽位**；
   预留失败则不进入调度队列。
2. Scheduler 在并发上限内为 Assignment 创建 Work，交给 WorkPool 执行。
3. WorkPool 执行完成后广播 `WorkResultReady`，工作阶段经 `WorkUpdated` 实时推送前端。
4. ResultAggregator 串行处理，把**当前已到达且已登记**的多个 Result 按 Task 合并为
   一次 `ResearchState.Commit`（不设人为等待窗口）。
5. Commit 成功后才回调 `Scheduler.OnResultCommitted`，Scheduler 据此把 Assignment
   置为 `completed` 并回收调度资源；失败则置 `failed` 并保留槽位以便重试。
6. Commit 成功后发布 `ResearchStateCommitted`，SupervisorLoop 被抢占并开启新一轮决策；
   若仍有未完成 Assignment，本轮不宣告"研究完成"。

**Work 内的 Mock 执行**：`workpool.MockWorker` 不接入真实模型，按固定阶段
（planning → retrieving → analyzing → summarizing）推进并产出确定性研究产物，
用于打通最小链路。阶段变化即前端展示的"调用信息"。

**全局中止语义**：Scheduler 把未完成 Assignment 标记 `cancelled`、
WorkPool 把未完成 Work 标记 `cancelled`、ResultAggregator 丢弃待提交结果。
当前阶段**只做状态标记，不做真实执行中断**。

**研究终结语义**：只有两种方式能让研究结束——

| 方式 | 触发 | 结果 |
|---|---|---|
| 模型主动宣布 | 调用 `finish_research` 工具 | `finished`；同时取消全部后台任务 |
| 用户中止 | `POST /sessions/:id/stop` | `stopped`；同时取消全部后台任务 |

模型宣布结束后**不可撤销**：Loop 拒绝新的推理输入，Runtime 停止 SupervisorLoop
并关闭提交门控。`finish_research` 不参与审批（它只是决策，无外部副作用）。

**委派语义**：模型通过 `submit_assignments` 工具决定"研究什么"——

```json
{"assignments":[
  {"goal":"收集与该主题相关的基础事实"},
  {"goal":"分析该主题的关键权衡与约束"}
]}
```

- 派发几个、目标是什么**完全由模型决定**，Runtime 不做轮次限流；
- 工具立即返回，子任务在后台异步执行，模型可立刻结束本轮；
- 子任务结果提交后唤醒下一轮，模型据此继续决策或宣布结束；
- 单次调用最多 8 个子任务、单个目标不超过 500 字符，超出部分会被拒绝并回告模型。

**撤销语义**：研究结束前，模型可用 `cancel_assignments` 纠正自己的派发决定——

```json
{"assignment_ids":["<id>"],"reason":"与另一个子任务重复"}
```

子任务清单（含 Assignment ID 与状态）会随上下文注入，模型据此引用 ID。
撤销按当前状态分别收尾：

| 撤销时的状态 | 处理 |
|---|---|
| 待调度 / 已入队 | 从调度队列移除 |
| 执行中 | 取消对应 Work |
| 等待提交 | 结果被提交门控拦下，不进入 ResearchState |
| 已完成（已提交） | 从 ResearchState 中撤回 |

门控是**按 Assignment 粒度**的（二级索引 `runTaskID → assignmentID`），
先标记撤销状态、再由提交侧拦截，即使槽位已删除也能拦住重复/迟到结果。

**提交历史**：提交与撤销都是"提交动作"，全部记录在 `ResearchState.CommitLog`
（只追加，含序号、操作类型、Assignment/Result、原因与版本号），
前端 ResearchState 面板可查看，接口为 `GET /sessions/:id/research-state`。

**被打断的原文**：一轮输出被新提交打断时，未完成的片段暂存于 `InterruptedContext`。
只要该字段非空，上下文就会渲染"上一轮被中断的输出"，使模型重入时能看到中断前的实际内容；
Commit 时原文并入本轮 Finding，随后清空。用户中止（`stopped`）不会 Commit，
因此残留原文会通过 `RelatedNode.InterruptedContext` 传给下一个研究节点，不会丢失。

**Assignment 归属 RunTask**：RunTask 消失则其下 Assignment 与 Work 一并删除
（`DeleteByRunTask` 级联），不会留下孤儿数据。

**配置**：

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `DRA_SCHEDULER_MAX_CONCURRENCY` | 2 | Scheduler **在途** Work 上限（排队 + 执行），提供背压 |
| `DRA_WORKPOOL_WORKERS` | 2 | WorkPool 的执行槽位数（真正并行的子 Agent 数） |
| `DRA_WORK_TIMEOUT` | 300 | 单个 Work（子 Agent 一次执行）的超时秒数 |
| `DRA_TOOL_REQUIRE_APPROVAL` | `*` | 需审批的工具名；`-` 或 `none` 显式关闭审批 |
| `DRA_DEBUG_ERRORS` | `false` | 为 `true` 时错误响应会附带内部细节（仅本地排查用） |

**两级并发控制**：两个参数限制的对象不同，需要配合设置。

| 参数 | 计数范围 | 作用 |
|---|---|---|
| `MaxConcurrency` | 已提交但未结束的 Work（**含排队**） | 背压，防止 WorkPool 队列无限增长 |
| `Workers` | 正在执行的 Work | 决定实际并行度 |

- `MaxConcurrency < Workers`：部分 Worker 会闲置，启动时打印警告；
- `MaxConcurrency > Workers`：WorkPool 队列会有等待项，属正常；
- 常用配置：两者相等，或 `MaxConcurrency` 略大于 `Workers`。

**子 Agent 执行模型**：每个 Work 由 `SubAgentFactory` 创建**独立的** `ChatModelAgent`
（单次任务型，不用 TurnLoop），与 Supervisor 共用同一个模型实例。

- Instruction 按 Work 的 Goal 生成，子 Agent 只围绕自己的目标工作；
- 工具白名单排除 `submit_assignments` / `cancel_assignments` / `finish_research`，
  子 Agent 不能派发子任务、撤销或结束研究；
- 子 Agent 的全部 assistant 输出累积为待提交的 Result；
- 超时与取消都会**显式 cancel** 子 Agent 的 ctx（只停止消费事件流不会让它停下）；
  超时标记 Work 为 `timedout` 且**不提交任何结果**，取消标记为 `cancelled`；
- 子 Agent **不参与任何人工审批**：工厂不注入审批中间件，
  审批只作用于 Supervisor 的工具调用。

**查询接口**：`GET /api/v1/sessions/:id/background-tasks` 返回当前 RunTask 下的
Assignment 与其最新 Work（状态、阶段、Worker、耗时）。

**错误处理**：完整清单见 [docs/ErrorHandbook.md](docs/ErrorHandbook.md)，
实现位于 `internal/fault`。要点如下：

- 统一错误对象携带 `Code` / `Layer` / `Severity` / `Source` / 影响范围，
  并保留 `errors.Is`/`errors.As` 语义；
- `fault.Wrap` **幂等**：已包装过的错误不会被外层重新标记，
  "最早、最了解情况"的那一层决定分类；
- 6 个层级（infrastructure / work / scheduler / result / runtime / api）
  表达"谁该处理"，`Severity`（info / warning / error / fatal）表达"该不该打扰用户"；
- 错误上报走 `fault.Reporter`：写日志 + 按 Severity 决定是否推送前端；
  同类错误在 5 秒窗口内合并，用 `count` 表达发生次数；
- API 层统一用 `writeError` 映射：`fault.Error` 按其 Code 映射状态码并只返回
  `ClientMessage`，未包装错误一律 500 + `internal error`，不泄漏内部细节。

**实时推送（两个 SSE 端点，职责不同）**：

| 端点 | 生命周期 | 推送内容 |
|---|---|---|
| `POST /sessions/:id/research` | 跟随单次研究请求，结束即关闭 | `message` / `tool_call` / `tool_result` / 审批事件 |
| `GET /sessions/:id/events` | **常驻**，切换会话才重建 | `run_status` / `assignment_updated` / `work_updated` |

`/events` 是**状态徽章与后台任务面板的实时数据源**：

- 连接建立后立即推送一次当前 `run_status`，前端无需等待下次变更；
- 任何 RunTask 状态迁移（running / waiting / stopped / finished）都会推送，
  且带 `previous` 字段；
- 研究在后台推进时（研究流已关闭）徽章依然实时变化；
- 前端仅在长连接断开、自动重连期间回退到轮询兜底。

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
| `DRA_TOOL_REQUIRE_APPROVAL` | 需审批的工具名，逗号分隔；`*` 表示全部（默认）；`-` 或 `none` 显式关闭 |

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
- 右侧上：**后台任务面板**（子 Agent 实时信息）——状态圆点、目标、阶段标签、
  Worker 标识、尝试次数、耗时；由 SSE `work_updated` 增量刷新，轮询兜底
- 右侧下：ResearchState 面板（需求 / 节点序号 / 版本 / Finding / Result / 中断上下文 / 前序节点）
- 头部：RunTask 状态徽章（研究中 / **等待中** / 已中止 / 已完成），每 800ms 轮询刷新
- 底部：输入框 + 主按钮 + 中止。主按钮随状态切换：
  - `running` → 发送
  - `waiting` → **继续**（切回运行，会把其他会话挤成 `waiting`）
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

「API 配置」支持设置服务地址、模型提供方（mock / mock-tool / mock-cancel / openai）、Base URL、
模型名称与 API Key；保存后立即生效。密钥留空表示沿用已保存的密钥，界面只展示脱敏提示。

**保存前会先探测连通性**（`POST /api/v1/config/verify`）：

- 前端点击确认时先探测，**探测失败则不允许保存**（弹窗保持打开并显示原因）；
- 后端 `PUT /config` 也会在写库前探测，避免绕过前端直接提交无效配置；
- mock 类提供方不涉及网络，直接视为可用；
- 错误分类：鉴权失败 → `invalid_input`（提示检查密钥），
  网络/超时 → `model_unavailable`（提示检查地址），模型名不可用 → `invalid_input`；
- 探测超时 10 秒（`llm.DefaultPingTimeout`），失败不会改动现有配置。

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
