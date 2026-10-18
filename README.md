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

# 开启新对话：建立新 ResearchState，并把上一对话结论写入 RelatedFinding
Invoke-RestMethod -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/conversations `
  -Method Post -ContentType "application/json" -Body '{"requirement":"新研究需求"}'
```

> 约束：
> - **一个 Session 只对应一个 Task**，重复创建返回 `409 Conflict`。
> - **Task 只能启用/禁用，无直接删除路径**；新建的 Task 默认禁用。
> - **RunTask 由独立实体 Runtime 经事件订阅维护**：启用/禁用/删除都先由 Runtime
>   向依赖组件（Supervisor/Scheduler/WorkPool，当前为 Mock）发起协商，协商通过后才原子落地。
> - **使能时若 RunTask 不存在，Runtime 先协商创建（含初始化要求）再新建 RunTask**，
>   同时初始化该 Session 的 ResearchState（首轮仅含研究需求）并启动 SupervisorLoop。
> - **SupervisorLoop** 外层订阅用户消息事件，内层用 Eino 的 TurnLoop 驱动模型产出研究结论，
>   写回 ResearchState；流式输出经 ModelOutput 事件发布为 SSE。
> - **ResearchState Commit 门控**：中止时停止接收新 Commit（返回 ErrNotAccepting），
>   正在进行的 Commit 继续完成；恢复后重新接受。
> - **用户主动中止（stop）为协作式/全局**：Runtime 与依赖组件协商后停止 SupervisorLoop、
>   置 RunTask 为 `stopped`、关闭提交门控；之后用户再次输入会自动恢复研究并重建上下文。
> - ResearchState 生命周期与 Session 绑定；Session 级联删除时一并删除 ResearchState。

## 前端模板

`web/index.html` 是一个零依赖的单文件控制台，用于检验 SSE 流式输出效果：

1. 启动服务（默认监听 `127.0.0.1:8080`，已开启 CORS）。
2. 用浏览器直接打开 `web/index.html`（`file://` 方式即可，服务端已允许跨源）。
3. 依次：创建 Session → 创建 Task → 启用 → 「研究（流式）」→ 观察 SSE 逐块输出；
   可再测试「中止研究」与「开启新对话」。

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
