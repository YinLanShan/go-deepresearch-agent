# 简历素材：DeepResearch Agent

> 项目：`DeepResearch Agent`（Go + CloudWeGo Eino + Hertz + PostgreSQL）
> 规模：生产代码 ~10.8k 行 / 测试 ~8.2k 行 / 17 个 internal 包 / 247 个测试函数 / 37 次提交
> 仓库：https://github.com/YinLanShan/go-deepresearch-agent

---

## 0. 使用建议

本文提供**三档篇幅**，按投递场景选择：

| 档位 | 篇幅 | 适用 |
|---|---|---|
| **A. 一行版** | 1 行 | 简历项目列表 |
| **B. 精简版** | 4-6 条 | 简历正文（推荐） |
| **C. 详细版** | 完整章节 | 面试讲解 / 作品集 / 技术博客 |

后面还附了 **STAR 讲法**、**面试高频追问与答案**、**可量化亮点清单**，
都是面试时真正会被问到的点。

---

## A. 一行版

> **DeepResearch Agent** — 基于 Go 的多智能体深度研究系统，用 Eino 的 TurnLoop 实现 Supervisor/SubAgent 双层架构，支持子任务并行、结果撤销、中断恢复与 PostgreSQL 持久化。

---

## B. 精简版（推荐用于简历正文）

**DeepResearch Agent** ｜ 个人项目 ｜ Go / CloudWeGo Eino / Hertz / PostgreSQL

- 设计并实现**双层 Agent 架构**：Supervisor 负责任务拆解与汇总（ReAct + 工具治理），SubAgent 独立执行子任务，通过 Scheduler→WorkPool→ResultAggregator 三级流水线实现真正的并发执行与背压控制。
- 攻克**长生命周期 Agent 的状态管理难题**：区分"本轮结束"与"研究结束"两种状态语义，实现结果提交时的抢占、子任务全部终结时的空闲唤醒、以及用户消息的高优先级插队与中断上下文保留。
- 建立**统一错误处理体系**：6 层错误分层（基础设施/执行/调度/结果/运行时/API）× 4 级严重度，幂等 Wrap 保证"最早知情层"决定分类，同类错误 5 秒窗口合并上报，避免前端刷屏。
- 完成 **PostgreSQL 持久化改造**：设计 11 张表的双粒度数据模型（ResearchState 归属 Task、Assignment 归属 RunTask），实现 Commit/Revoke 全事务化、`DELETE...RETURNING` 撤销审计、以及 8 并发无丢失更新的行锁方案。
- 建立**存储契约测试**：同一套断言同时验证内存与 PG 两套实现，保证"换数据库不改变行为"；配套 67 个 PG 集成测试与 247 个全项目测试，CI 全绿。

---

## C. 详细版

### C.1 项目定位

一个**长生命周期的多智能体研究系统**：用户提出研究目标后，系统自主拆解为多个子任务、
并行执行、逐步汇总，最终输出研究结论。与常见的"一问一答"式 Agent 不同，
本项目的核心挑战在于 **一次研究会持续数十轮、涉及多个并发子任务、且必须可中断可恢复**。

技术栈：Go 1.27 / CloudWeGo Eino v0.9（Agent 框架）/ Hertz（HTTP+SSE）/ PostgreSQL 18 / pgx v5。

### C.2 核心设计

#### 架构分层

```
用户消息（抢占优先级最高）
    ↓
Supervisor（Eino TurnLoop，ReAct 循环）
    ├─ submit_assignments → 派发子任务
    ├─ cancel_assignments → 撤销（含已提交结果）
    └─ finish_research    → 终结研究，写 Conclusion
    ↓
Scheduler（在途上限 + 背压）
    ↓
WorkPool（N 个独立 SubAgent 真并行执行）
    ├─ 超时 → 立即终止，结果丢弃
    └─ 取消 → cancel ctx 中断执行
    ↓
ResultAggregator（按 Assignment 粒度提交门控）
    ↓
ResearchState（Requirements/Results/Findings/Conclusion/CommitLog）
```

#### 关键设计决策（面试重点）

**1. "本轮结束" vs "研究结束" 的语义分离**

大多数 Agent 实现把"这轮回复完了"当作"任务结束"。本项目明确区分：

| 事件 | 含义 | 系统行为 |
|---|---|---|
| 本轮结束 | 模型暂时没话说了 | RunTask 保持 `running`，Loop 存活等待唤醒 |
| 研究结束 | 模型主动调用 `finish_research` | 取消全部子任务、关闭提交门控、进入终态 |

如果混为一谈，Supervisor 会在子任务还没回来时就退出，研究永远无法汇总。

**2. 两种唤醒机制**

- **结果提交唤醒**（抢占）：子任务提交结果时，若 Supervisor 空闲则唤醒它。
- **全部终结唤醒**：所有 Assignment 进入终态时唤醒，避免"全部子任务都失败了但 Supervisor 不知道"。

第二个机制是实际跑代码时发现的缺口——最初只实现了前者，
在"子任务全部失败"的路径上 Supervisor 会永久空闲。

**3. 撤销的"先标记后拦截"**

撤销一个子任务时，A 和 B 可能同时发生：

```
A: 模型调用 cancel_assignments 撤销 assignment-1
B: assignment-1 的 Work 正好执行完，提交结果
```

解决方案是**先标记撤销状态，再判断是否拦截提交**，而不是反过来。
否则 B 可能抢在标记之前提交成功，导致撤销失效。

**4. 子 Agent 不需要审批**

最初子 Agent 也走审批中间件，但子任务是后台自动执行的，没有人在旁边点"同意"。
现在子 Agent 完全不注入审批中间件，超时直接终止且**不提交结果**。

**5. 结果必须是独立的 slice 元素**

曾试图把多个 Result 拼成一个字符串，这会让"按 Assignment 撤销"变得不可能
（无法从拼接字符串里精确删除某一条）。改成独立元素后，撤销就是一次 `DELETE ... WHERE assignment_id = $1`。

#### 错误处理体系

**两个正交维度**：

- **Layer**（谁该处理）：infrastructure / work / scheduler / result / runtime / api
- **Severity**（该不该打扰用户）：info / warning / error / fatal

**幂等 Wrap** 是核心设计：

```go
func (b *Builder) Wrap(err error) *Error {
    var existing *Error
    if errors.As(err, &existing) {
        return existing      // 已包装过则原样返回
    }
    b.e.Err = err
    return b.e
}
```

保证"最早、最了解情况"的那一层决定分类，而不是被外层反复覆盖。

配套实现：
- **Reporter**：写日志 + 按 Severity 决定是否推送前端，同类错误 5 秒窗口合并（`count` 表达次数）；
- **API 统一映射**：`writeError` 按 Code 映射状态码，未包装错误一律 500 + `internal error`，**不泄漏内部细节**。

#### 持久化设计

**核心难点是 ResearchState 的表结构设计。**

它有 6 个变长集合，其中 `Results` 必须支持**按 AssignmentID 精确删除**（撤销功能）。
判断标准是：**需要按元素级增删改 → 拆表；只需整体读写 → 集合类型列。**

| 集合 | 需求 | 决定 |
|---|---|---|
| Results | 按 assignmentID 精确删除 | 拆表 |
| Findings | 只追加 | 拆表 |
| CommitLog | 审计需按 op/时间查 | 拆表 |
| History | 可能很长，前端分页 | 拆表 |
| Requirements | `[]string`，只追加 | **`TEXT[]`** |
| RelatedNodes | 嵌套结构，只读快照 | `JSONB` |

**归属粒度必须区分**：

```
ResearchState  → 归属 Task      （跨 RunTask 存活）
Assignment     → 归属 RunTask   （RunTask 消失即消失）
```

这是业务语义决定的，不能统一成同一个外键。

**两个实现层面的陷阱（都是测试暴露的）**：

1. **定长 CHAR 会补空格**：PG 的 `CHAR(32)` 取出可能带尾部空格，导致 Go 侧 map 查找失败。全部改用 `TEXT` + `CHECK (length(id) = 32)`。
2. **同毫秒插入的顺序不稳定**：Commit 在一个事务内批量插入多条 Result，`created_at` 完全相同，按 `id`（随机 hex）排序顺序不确定。新增 `seq BIGINT GENERATED ALWAYS AS IDENTITY` 列，读取按 seq 升序。

### C.3 工程质量

| 措施 | 说明 |
|---|---|
| **契约测试** | `internal/storage/storetest` 用同一套断言验证内存与 PG 实现，`Capabilities` 声明已知差异（当前全为 false） |
| **集成测试** | 67 个 PG 测试，通过 `DRA_TEST_PG_DSN` 门控（未设置自动 skip，不影响无 PG 环境） |
| **并发测试** | 4 个并发用例 + `-race` 检测，验证 `FOR UPDATE` 行锁无丢失更新 |
| **错误手册** | `docs/ErrorHandbook.md` 汇总 47 个错误条目与处理状态 |
| **数据模型文档** | `docs/DataModel.md` 作为实现前的契约，含建表 SQL 与语义对齐清单 |

### C.4 遇到并解决的典型问题

| # | 问题 | 排查与修复 |
|---|---|---|
| 1 | **WorkPool 不是真并行** | 共享单例 Worker，WorkerID 互相覆盖 → 改为工厂模式，每个 Work 独立 Agent |
| 2 | **中断原文只存不渲染** | 模型重入时看不到自己写到哪 → RelatedNode 补充 InterruptedContext 字段并渲染 |
| 3 | **全部终结不唤醒** | 子任务全失败时 Supervisor 永久空闲 → 新增 AllAssignmentsSettled 事件 |
| 4 | **事件从不投递** | Loop 的 Send 里写了 case 但漏了 Bus.Subscribe → 补订阅 |
| 5 | **mock 无限派发** | 唤醒后历史无痕迹，每轮重复派发 → 在 instruction 里检查子任务清单 |
| 6 | **取消后仍推进度** | 前端收到 cancelled 后又收到阶段倒退 → 加 stale 标记丢弃迟到进度 |
| 7 | **API 直出 err.Error()** | 20 处泄漏内部细节 → 统一 writeError 脱敏 |
| 8 | **槽位失败静默跳过** | Assignment 成僵尸任务（无终态） → 标记 cancelled 并上报 |
| 9 | **配置保存不验证** | 错误密钥也能保存成功 → 保存前 ping 一次 |
| 10 | **Turn 基准不一致** | AppendResult 用 len(Results)、Commit 用 len(Findings)，轮次跳号 → 统一基准 |

第 10 条是**用户 code review 发现的**，我在移植 PG 实现时"严格对齐内存实现"照搬了这个缺陷，
说明"与旧实现一致"不能替代"语义正确"的判断。

---

## D. STAR 讲法（面试用）

### S（情境）

想做一个能真正做"深度研究"的 Agent，而不是一问一答的客服机器人。
难点在于：一次研究要跑几十轮、并发多个子任务、还要能中断恢复。

### T（任务）

设计并实现这个系统，同时解决长生命周期 Agent 的状态管理、并发控制、错误处理和持久化。

### A（行动）

1. 选了 Eino 的 TurnLoop 而不是 Runner，因为 Runner 是一次性的，而我们需要一个长期存活的循环；
2. 明确区分"本轮结束"与"研究结束"，用两种唤醒机制驱动；
3. 三级流水线（Scheduler / WorkPool / ResultAggregator）分离调度、执行、聚合；
4. 错误处理做成 6 层 × 4 级的二维模型；
5. 持久化时按"元素级增删改"标准决定拆表还是用集合类型；
6. 用契约测试保证两套存储实现行为一致。

### R（结果）

- 10.8k 行生产代码 + 8.2k 行测试，247 个测试函数全绿；
- PG 模式下端到端验证：完整研究落库，重启后数据完整保留；
- 沉淀了错误手册与数据模型文档，为后续扩展提供契约。

---

## E. 面试高频追问与答案

### Q1：为什么选 Eino 的 TurnLoop 而不是 Runner？

Runner 是"一次性执行到结束"的模型，适合单轮任务。我们的 Supervisor 需要
**长期存活、被反复唤醒、在两次输入之间保持上下文**，TurnLoop 正是为此设计的
（`GenInput` / `GenResume` / `Preempted`）。用 Runner 会导致每轮都重建 Agent。

### Q2：怎么保证并发安全？

三道防线：

1. **存储层**：`SELECT ... FOR UPDATE` 行锁，保证 version 自增不丢失；并发测试 + `-race` 验证；
2. **聚合层**：ResultAggregator 单 goroutine 串行提交，天然无竞争；
3. **状态机**：Assignment 状态流转集中在一处，避免多处修改。

### Q3：撤销和提交并发时怎么保证一致性？

**先标记撤销状态，再判断是否拦截提交**。同时 `RevokeResult` 用
`DELETE ... RETURNING` 一条语句完成"删除 + 取回被删内容"，内容写入 CommitLog 保留审计，
全程一个事务。

### Q4：为什么不用乐观锁？

写操作很短（几条 INSERT），冲突也少（单 goroutine 串行），
`FOR UPDATE` 语义简单且无需重试循环。乐观锁的优势场景是"写事务长、冲突多"，不适用这里。

### Q5：会话重启后能恢复吗？

数据能恢复（PG 持久化已端到端验证），但**提交门控是进程内状态**，
重启后重置为"全部开启"——这是已知限制，需要启动时依据 RunTask 状态重建。

### Q6：测试怎么保证两套存储行为一致？

`internal/storage/storetest` 提供契约测试，两套实现跑同一批断言（10 个子测试）。
任何一边改了语义，另一边立刻挂。`Capabilities` 用于声明已知差异，当前全为 false。

### Q7：这个项目有多少是自己写的？

全部代码由我设计并实现，AI 助手作为结对编程工具参与。
我负责架构决策、语义定义、缺陷判断，AI 负责具体编码与测试编写。
**关键的设计判断（如"本轮结束 vs 研究结束"、拆表标准、错误分层）都是我的决定。**

---

## F. 可量化亮点清单（挑 3-5 条放进简历）

- 10781 行生产代码，8242 行测试代码，**测试占比 43%**
- 247 个测试函数，17 个包全绿
- PostgreSQL 集成测试 67 个 + 契约测试 10 个 × 2 套
- 4 个并发测试 + `-race` 检测通过
- 11 张表的数据模型，含双粒度归属设计
- 6 层 × 4 级的错误分类体系，47 个错误条目
- 37 次提交，每次都有完整说明
- 端到端验证：PG 模式跑通完整研究，重启后数据完整保留

---

## G. 简历排版示例

```
DeepResearch Agent                                             2025
个人项目 · Go / CloudWeGo Eino / Hertz / PostgreSQL
https://github.com/YinLanShan/go-deepresearch-agent

· 设计双层 Agent 架构（Supervisor + SubAgent），通过 Scheduler/WorkPool/ResultAggregator
  三级流水线实现子任务并发执行与背压控制，解决长生命周期 Agent 的状态管理问题
· 明确区分"本轮结束"与"研究结束"语义，实现结果抢占唤醒与全部终结唤醒两种驱动机制，
  修复子任务全部失败时 Supervisor 永久空闲的缺陷
· 建立 6 层 × 4 级错误处理体系，幂等 Wrap 保证最早知情层决定分类，同类错误
  5 秒窗口合并上报，API 层统一脱敏
· 完成 PostgreSQL 持久化改造：11 张表双粒度数据模型，Commit/Revoke 全事务化，
  行锁保证 8 并发无丢失更新；建立存储契约测试保证两套实现行为一致
· 10781 行生产代码 + 8242 行测试（247 个测试函数），全项目测试通过
```

---

## H. 需要准备的短板问题

诚实面对可能的追问：

| 追问 | 建议回答 |
|---|---|
| 有真实用户吗？ | 没有，是个人技术验证项目。目标是完整实现一套生产级的多智能体架构。 |
| 用真实 LLM 跑过吗？ | 端到端用的是 mock 模型（确定性、可复现）。真实 LLM 接口已接入并通过连通性探测，但没有大规模压测。 |
| 性能数据？ | 没有做压测。当前重点是语义正确性与数据一致性。 |
| 为什么不用现成的框架？ | 想自己实现调度与状态管理，理解 Agent 系统的核心难点。Eino 只用了它的 Loop 与工具治理机制。 |
| 有 CI 吗？ | 没有配置 CI，但有完整的本地测试流程（gofmt/vet/test）。这是后续要补的。 |
