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
```

> 约束：
> - **一个 Session 可对应多个 Task**；一个 Task 对应一个 RunTask。
> - **同一时间全局只有一个 RunTask 处于 Running**：使能新的 Task 前，Runtime 会先失能
>   其他运行中的 Session（其 RunTask 置 `paused`、Task 置不可用）。
> - **Task 只能启用/禁用，无直接删除路径**；新建的 Task 默认禁用。
> - **RunTask 由独立实体 Runtime 经事件订阅维护**：启用/禁用/删除都先由 Runtime
>   向依赖组件（Supervisor/Scheduler/WorkPool，当前为 Mock）发起协商，协商通过后才原子落地。
> - **使能时若 RunTask 不存在，Runtime 先协商创建（含初始化要求）再新建 RunTask**，
>   同时初始化该 Task 的 ResearchState 并启动 SupervisorLoop；若该 Session 已有上一份
>   ResearchState，则把其结论作为 `RelatedFinding` 继承。
> - **失能 = 全局中止 + 状态置 `paused`**：停止 SupervisorLoop、关闭提交门控；
>   再次使能（"继续"）由 `paused` 置回 `running` 并重新打开门控。
> - **用户主动中止 = 全局中止 + 状态置 `stopped`**：终止态，"继续"不会复活。
> - **模型完成研究 = 状态置 `finished`**：终止态。此后用户再发消息，会在该 Session 下
>   自动新建 Task 与新 ResearchState（上一份结论写入 `RelatedFinding`）并继续研究。
> - **SupervisorLoop** 外层订阅用户消息事件（按 RunTaskID 路由），内层用 Eino 的
>   TurnLoop 驱动模型产出研究结论，写回该 Task 的 ResearchState；流式输出经
>   ModelOutput 事件发布为 SSE。
> - **ResearchState Commit 门控**：中止/暂停时停止接收新 Commit（返回 ErrNotAccepting），
>   正在进行的 Commit 继续完成。
> - **模型可热切换**：`main` 持有 `llm.Swappable`，`PUT /api/v1/config` 只替换内部模型实例，
>   Runtime / SupervisorLoop / ChatService 无需重建。
> - ResearchState 归属 Task；Session 级联删除时一并删除其下全部 ResearchState。

## 前端控制台

`web/index.html` 是单文件前端（无构建步骤），布局参考 eino-examples 的 chatwitheino：

- 顶栏：`⚙ API 配置`、`+ 新建研究`
- 左侧：研究会话列表（可删除）
- 中间：卡片式聊天区，Supervisor 输出以 SSE 流式增量渲染（Markdown、自动滚动）；
  头部显示 RunTask 状态徽章（研究中 / 已暂停 / 已中止 / 已完成）
- 右侧：ResearchState 面板（需求 / 版本 / Finding / Result / 中断上下文 / RelatedFinding）
- 底部：输入框 + 主按钮 + 中止。主按钮随状态切换：
  - `running` → 发送
  - `paused` → **继续**（把 RunTask 由 paused 置回 running）
  - `finished` / `stopped` → 发送（发送后自动新建 Task 与新 ResearchState）

使用方式（二选一）：

1. **直接由服务提供**：启动服务后访问 `http://127.0.0.1:8080/`。
2. **以文件方式打开**：浏览器打开 `web/index.html`（`file://`），
   在「API 配置 → 服务地址」填写 `http://127.0.0.1:8080`（服务端已开启 CORS）。

「API 配置」支持设置服务地址、模型提供方（mock / openai）、Base URL、模型名称与 API Key；
保存后立即生效。密钥留空表示沿用已保存的密钥，界面只展示脱敏提示。

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
