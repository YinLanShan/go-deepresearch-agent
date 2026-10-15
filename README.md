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

# 删除 Session（会级联删除其名下的 Task）
Invoke-WebRequest -Uri http://127.0.0.1:8080/api/v1/sessions/{id} -Method Delete

# 删除 Session 下的 Task
Invoke-WebRequest -Uri http://127.0.0.1:8080/api/v1/sessions/{id}/tasks/{taskID} -Method Delete
```

> 约束：**一个 Session 只对应一个 Task**。对已有 Task 的 Session 再次创建 Task 会返回 `409 Conflict`。

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
