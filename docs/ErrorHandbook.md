# 错误手册（Error Handbook）

本手册是项目**所有错误类型**的权威清单，记录每一类错误的：
**产生位置 → 错误码 → 当前处理方式 → 是否需要你决策**。

- 设计依据：`docs/Event-Error-Interrupt.md` 第 9.3、12 节
- 实现位置：`internal/fault`（错误对象与上报）、`internal/api/errors.go`（HTTP 映射）
- 状态标记：✅ 已规范化 ｜ ⚠️ 待处理 ｜ ❓ 需你决策

---

## 0. 快速索引

| 层级 | 含义 | 谁该处理 | 典型场景 |
|---|---|---|---|
| `infrastructure` | 模型 / MCP / 检索等外部依赖 | 不决定研究计划，只提供原始信息 | 限流、网络失败 |
| `work` | SubAgent 单次执行 | 不擅自重试整个 Assignment | 超时、取消、空输出 |
| `scheduler` | 调度与资源分配 | 执行调度和资源策略 | 槽位不可用、创建 Work 失败 |
| `result` | 结果接收、聚合、提交 | 保证提交与版本正确 | Commit 失败、门控未开 |
| `runtime` | 生命周期、恢复、全局控制 | 协调执行环境与系统生命周期 | 门控、关闭、Task 状态 |
| `api` | HTTP 边界 | 参数校验与错误映射 | 参数非法、资源不存在 |

**Severity**（决定是否打扰用户）：

| 级别 | 含义 | 是否推送前端 |
|---|---|---|
| `info` | 辅助信息，不影响研究 | 否（只写日志） |
| `warning` | 局部失败，研究可继续 | 是 |
| `error` | 需要用户注意或调整 | 是 |
| `fatal` | 研究无法继续 | 是（额外写入顶部提示） |

---

## 1. infrastructure 层

### 1.1 模型提供方不支持

- **位置**：`internal/llm/llm.go:98`
- **错误码**：`invalid_input` ｜ Severity：`error`
- **现状**：✅ 已用 `fault.New`，Layer=infrastructure
- **说明**：配置里写了不支持的 provider 名（非 mock/mock-tool/openai）

### 1.2 模型不可用（连接 / 鉴权失败）

- **位置**：`internal/llm/openai.go:24`
- **错误码**：`model_unavailable` ｜ Severity：`error` ｜ RetryableHint：建议 true
- **现状**：✅ 已用 `fault.Wrap`
- **说明**：构造 OpenAI 兼容客户端失败

### 1.3 模型流式调用失败

- **位置**：`internal/service/chat.go:40` / `:50`
- **错误码**：`model_unavailable` ｜ Severity：`error`
- **现状**：✅ 已用 `fault.Wrap`
- **说明**：`model stream` / `model recv` 两处

### 1.4 模型限流

- **位置**：待接入（当前无独立处理）
- **错误码**：`rate_limited` ｜ Severity：`warning` ｜ RetryableHint：true
- **现状**：⚠️ **错误码已定义但未使用**
- **说明**：设计文档要求 Infrastructure 报告 `rate_limited` 而非在上层无限重试
- **❓ 需你决策**：见第 8 节 Q1

### 1.5 工具调用失败（MCP / 内置）

- **位置**：`internal/tool/*`、Supervisor 工具执行
- **错误码**：`tool_failed` ｜ Severity：`warning`
- **现状**：⚠️ 工具错误由 `SafeToolMiddleware` 转为文本回给模型，**不产生 fault 错误**
- **说明**：这是有意设计（让模型自行看到错误并调整），但前端完全不可见
- **❓ 需你决策**：见第 8 节 Q2

### 1.6 MCP 工具加载失败

- **位置**：`internal/tool/registry.go:73`
- **现状**：⚠️ 只 `slog.Warn`，返回 nil 工具列表
- **建议**：`tool_failed` + Severity=`warning`
- **说明**：某个 MCP Server 连不上，其它工具仍可用

### 1.7 内置工具注册失败

- **位置**：`internal/tool/registry.go:83`
- **现状**：⚠️ 只 `slog.Warn`
- **建议**：`tool_failed` + Severity=`warning`

---

## 2. work 层

### 2.1 Work 超时

- **位置**：`internal/workpool/workpool.go`（`run` 中判断 `context.DeadlineExceeded`）
- **错误码**：`work_timeout` ｜ Severity：`warning`
- **现状**：✅ 状态置 `timedout`、结果丢弃、不提交
- **说明**：超时即终止，**不进人工审批**（已按你的决定调整文档）
- **待补**：尚未通过 `fault.Reporter` 上报前端

### 2.2 Work 被取消

- **位置**：`internal/workpool/workpool.go`（`context.Canceled`）
- **错误码**：`work_canceled` ｜ Severity：`info`
- **现状**：✅ 状态置 `cancelled`
- **说明**：取消是控制流程的结果，不应作为普通失败；**默认不该打扰用户**

### 2.3 SubAgent 执行失败

- **位置**：`internal/workpool/subagent_worker.go`（`agent.Run` 返回非 ctx 错误）
- **错误码**：`work_failed` ｜ Severity：`warning`
- **现状**：⚠️ 状态置 `failed`，`Err` 字段存原始字符串
- **建议**：用 `fault.Wrap` 包装后上报

### 2.4 SubAgent 空输出

- **位置**：`internal/workpool/subagent_worker.go`（`ErrSubAgentEmptyResult`）
- **错误码**：`work_empty_result` ｜ Severity：`warning`
- **现状**：⚠️ 原生 `errors.New`
- **说明**：子 Agent 没产出任何文本

### 2.5 SubAgent 意外中断（需审批）

- **位置**：`internal/workpool/subagent_worker.go`（`ErrSubAgentNeedsApproval`）
- **错误码**：`work_needs_approval` ｜ Severity：`error`
- **现状**：⚠️ 防御性兜底——子 Agent 不注入审批中间件，正常不会触发
- **说明**：若出现说明有非预期组件触发中断

### 2.6 Work 无执行器

- **位置**：`internal/workpool/workpool.go`（工厂未配置 / 返回 nil）
- **错误码**：`work_failed` ｜ Severity：`fatal`
- **现状**：⚠️ 原生错误，Work 置 `failed`
- **说明**：装配错误，属不可恢复

### 2.7 Work 状态更新失败

- **位置**：`internal/workpool/workpool.go` **6 处**
  （`starting`、`progress`、失败收尾、成功收尾、`finishFailure`、`markCancelled`）
- **错误码**：`work_failed` ｜ Severity：`error`
- **现状**：⚠️ 只 `slog.Error("workpool: update work failed")` 后继续
- **建议**：`RetryOnce` + `fault.Wrap`（对应批次 E）
- **❓ 需你决策**：见第 8 节 Q3

---

## 3. scheduler 层

### 3.1 结果槽位预留失败

- **位置**：`internal/scheduler/scheduler.go:386`（`enqueue`）
- **错误码**：`slot_unavailable` ｜ Severity：`error`
- **现状**：⚠️ `slog.Error` 后 `continue`——**该 Assignment 被静默跳过，不调度**
- **建议**：上报前端（用户会看到某个子任务凭空消失）
- **❓ 需你决策**：见第 8 节 Q4

### 3.2 Assignment 创建失败

- **位置**：`internal/scheduler/scheduler.go:396`
- **错误码**：`work_create_failed` ｜ Severity：`error`
- **现状**：⚠️ `slog.Error` 后 `continue`，并回收槽位

### 3.3 Work 创建失败

- **位置**：`internal/scheduler/scheduler.go:218`
- **错误码**：`work_create_failed` ｜ Severity：`error`
- **现状**：✅ 走 `fail()` → Assignment 置 `failed`

### 3.4 Work 提交到 WorkPool 失败

- **位置**：`internal/scheduler/scheduler.go:252`
- **错误码**：`work_create_failed` ｜ Severity：`error`
- **现状**：✅ 走 `fail()` → Assignment 置 `failed`

### 3.5 Assignment 状态更新失败

- **位置**：`internal/scheduler/scheduler.go` **10 处**
- **错误码**：`invalid_transition` / `internal` ｜ Severity：`error`
- **现状**：⚠️ 全部只 `slog.Error("scheduler: update assignment failed")` 后继续
- **说明**：这是**数量最多的一处隐患**——状态写不进存储但流程继续，
  前端看到的与实际不一致，重启后丢失
- **❓ 需你决策**：见第 8 节 Q3

### 3.6 读取 Assignment 失败（事件处理）

- **位置**：`internal/scheduler/scheduler.go:440`
- **现状**：⚠️ `slog.Error` 后 return，事件被丢弃

### 3.7 列出 Assignment 失败

- **位置**：`internal/scheduler/scheduler.go:764`（`maybePublishSettled`）
- **现状**：⚠️ `slog.Error` 后 return，**不会触发"全部终结"唤醒**
- **说明**：可能导致 Supervisor 永久空闲（正是刚修的那个缺口的回归路径）

### 3.8 撤销未知 Assignment / 跨 RunTask 撤销

- **位置**：`internal/scheduler/scheduler.go:649` / `:653`
- **现状**：✅ 只 `slog.Warn`，属合理（幂等 / 越权拒绝）

---

## 4. result 层

### 4.1 Commit 失败

- **位置**：`internal/resultagg/resultagg.go:388`
- **错误码**：`commit_failed` ｜ Severity：`error`
- **现状**：✅ 调用 `markRejected` → Scheduler 置 Assignment `failed`
- **说明**：设计文档要求**不得增加 StateVersion、不得宣告成功、不得回收 Assignment**，当前符合

### 4.2 提交门控已关闭

- **位置**：`internal/researchstate/memory/memory.go`（`ErrNotAccepting`）
- **错误码**：`not_accepting` ｜ Severity：`warning`
- **现状**：⚠️ 原生哨兵错误，由 API 层映射为 409
- **说明**：研究已结束或被挤掉时仍在提交

### 4.3 撤销已提交结果失败

- **位置**：`internal/resultagg/resultagg.go:238`
- **错误码**：`commit_failed` ｜ Severity：`**fatal**`
- **现状**：⚠️ 只 `slog.Error("resultagg: revoke committed result failed")`
- **说明**：ResearchState 里已提交的结果没被撤回，**数据与预期不一致**
- **❓ 需你决策**：见第 8 节 Q5

### 4.4 空结果

- **位置**：`internal/resultagg/resultagg.go`（`commitGroup`）
- **错误码**：`result_rejected` ｜ Severity：`warning`
- **现状**：⚠️ 返回 `fmt.Errorf("resultagg: empty result for task %s")`

### 4.5 写入结论失败

- **位置**：`internal/supervisor/loop.go:667`（`SetConclusion`）
- **错误码**：`commit_failed` ｜ Severity：`**fatal**`
- **现状**：⚠️ 只 `slog.Error("set conclusion failed")`
- **说明**：研究已终结但结论丢失
- **❓ 需你决策**：见第 8 节 Q5

---

## 5. runtime 层

### 5.1 提交门控（已自愈）

- **位置**：`internal/researchstate/memory/memory.go`（`StopCommits` / `ResumeCommits`）
- **现状**：✅ **已改为不返回 error**——门控是派生状态，条目不存在也直接记录
- **说明**：按你的决定（Q1=A），"门控丢失"这类无法处理的错误从根源消失

### 5.2 Task 失能写入失败

- **位置**：`internal/runtime/runtime.go:527`（`_ = r.tasks.Update`）
- **错误码**：`internal` ｜ Severity：`error`
- **现状**：⚠️ `_ =` 静默丢弃
- **建议**：`RetryOnce` + 忽略 `ErrNotFound`（Task 已删除是正常情况）

### 5.3 组件 Shutdown 失败

- **位置**：`internal/runtime/runtime.go:323/326/329`（Scheduler / WorkPool / Aggregator）
- **错误码**：`runtime_shutdown_failed` ｜ Severity：`warning`
- **现状**：⚠️ `_ =` 静默丢弃
- **说明**：进程退出时超时；不影响已提交数据，但可能残留 goroutine

### 5.4 SupervisorLoop Shutdown 失败

- **位置**：`internal/runtime/runtime.go:696/781/796`
- **错误码**：`runtime_shutdown_failed` ｜ Severity：`warning`
- **现状**：⚠️ `_ =` 静默丢弃

### 5.5 Checkpoint 删除失败

- **位置**：`internal/runtime/runtime.go:709`
- **现状**：✅ 已 `slog.Warn("delete checkpoint failed")`

### 5.6 清理 RunTask 执行数据失败

- **位置**：`internal/runtime/runtime.go:554`
- **现状**：✅ 已 `slog.Warn("purge run task execution failed")`

### 5.7 发布状态事件时 Task 不存在

- **位置**：`internal/runtime/runtime.go:661`
- **现状**：✅ 已 `slog.Warn`，合理（Task 被删）

### 5.8 事件处理失败

- **位置**：`internal/runtime/runtime.go:353`
- **现状**：✅ 已 `slog.Error("runtime handle event failed")`

### 5.9 握手确认失败

- **位置**：`internal/runtime/runtime.go:365`（`d.h.Confirmed(ctx)`）
- **现状**：⚠️ `_ =` 静默丢弃
- **说明**：协商握手的回执写失败

---

## 6. api 层

### 6.1 参数校验失败

- **位置**：`internal/api/api.go` 多处（`invalid request body` / `xxx is required`）
- **现状**：✅ 返回 400，格式 `{"error": "...", "code": "bad_request"}`

### 6.2 资源不存在

- **位置**：`requireSession` / 各 handler
- **现状**：✅ 返回 404 + `code: not_found`（由 `writeError` 统一映射）

### 6.3 业务冲突

- **位置**：`handleApprove` 等
- **现状**：✅ 返回 409 + `code: conflict`

### 6.4 未包装内部错误

- **位置**：任意 handler
- **现状**：✅ 统一返回 500 + `internal error`，**不泄漏内部细节**

---

## 7. 通知类（不产生 fault，保持 `_ =`）

这些属于"上报通道自身"，失败不改变业务状态，**明确保留 `_ =`**，
并在代码中以集中注释说明约定（见各 `publish` 辅助方法与 SSE `Send`）：

| 位置 | 内容 | 数量 |
|---|---|---|
| `internal/api/api.go` | SSE `WriteEvent` / `Close` | 21 |
| `internal/resultagg/resultagg.go` | `bus.Publish`（notifySupervisor） | 1 |
| `internal/runtime/runtime.go` | `bus.Publish`（状态变更） | 1 |
| `internal/scheduler/scheduler.go` | `bus.Publish` | 1 |
| `internal/supervisor/loop.go` | `bus.Publish` × 4 | 4 |
| `internal/workpool/workpool.go` | `bus.Publish` | 1 |
| `internal/tool/mcp.go` | `session.Close()` | 1 |
| `cmd/server/main.go` | `rt.Shutdown` | 1 |
| `cmd/mcptestserver/main.go` | `srv.Close()` | 1 |

**合计 32 处**。

判断标准：**失败后是否需要改变业务状态？**

- 不需要 → 通知类，保留 `_ =` 并注释；
- 需要（如门控、状态落库）→ 必须处理（重试 / 上报 / 作废）。

占位残留（`_ = now` / `_ = taskID`）已清理完毕。


---

## 8. 决策结果（已确认）

### Q1：模型限流单独识别 + 可配置重试 ✅

**决定**：只重试 `rate_limited` / `model_unavailable`，在 **llm 层包装**，
按固定间隔重试若干次；次数用完仍未成功则：
向前端反馈 → 清理实体 → 失能 RunTask → 返回 error。

**配置**：`DRA_MODEL_MAX_RETRIES`（默认 2）、`DRA_MODEL_RETRY_INTERVAL`（默认 2 秒，秒）。

**实现要点**：
- 重试由 `llm` 层的包装器完成，对上层透明；
- 只在可重试错误码上重试，其余错误立即返回（避免掩盖真实问题）；
- 重试耗尽后由 Supervisor/Runtime 走"研究失败"路径。

### Q2：工具失败上报 warning ✅

**决定**：`SafeToolMiddleware` 在把错误转成文本给模型的同时，
上报 `warning` 级别的 `ErrorOccurred`（`tool_failed`），前端显示但不阻断。

### Q3：状态更新失败 → 重试一次 → 作废 Assignment ✅

**决定**：状态更新失败先 `RetryOnce`；仍失败则：
- 上报 `error`；
- 将该 Assignment 标记为 `failed` 并**作废**
  （该任务继续存在大概率导致数据不一致，因此不再推进、结果不提交）；
- 同一 RunTask 的其它 Assignment 不受影响。

**语义**：`failed` = 执行终止 + 作废；与 `cancelled`（主动撤销）语义不同但同为终态。

### Q4：槽位预留失败 → 标记 cancelled ✅

**决定**：`enqueue` 预留槽位失败时，把该 Assignment 标记为 `cancelled` 并上报 `error`，
而不是静默 `continue`（避免出现"永远不调度、也没有终态"的僵尸任务）。

### Q5：结论 / 撤销写入失败 → fatal，不阻断，放弃 ✅

**决定**：`SetConclusion` / `RevokeResult` 失败上报 `fatal` 级别，
**不阻断也不回滚**（研究已终结，无法回退），随后放弃这次写入。

### Q6：组件 Shutdown 失败 → 仅日志 ✅

**决定**：不发布事件、不上报前端，只写日志（进程即将退出，前端可能已断开）。

---

## 9. 待处理批次汇总

| 批次 | 内容 | 数量 | 决策 |
|---|---|---|---|
| **A/B/C** | 错误对象、上报、API 映射 | —— | ✅ 已完成 |
| **D** | 状态变更类 `_ =`（runtime 层） | 6 处 | ✅ Q6 |
| **E** | 状态更新失败 + `RetryOnce` → 作废 | 16 处 | ✅ Q3 |
| **F1** | work 层接入 fault | 6 类 | ✅ |
| **F2** | scheduler 层接入 fault | 7 类 | ✅ Q4 |
| **F3** | result 层接入 fault | 5 类 | ✅ Q5 |
| **F4** | infrastructure 层 + 模型重试 + 工具上报 | 7 类 | ✅ Q1、Q2 |
| **G** | 清理残留 + 通知类补注释 | 32 处 | ✅ |

**全部批次已完成。**


