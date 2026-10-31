# 简历上的项目呈现（DeepResearch 主项目）

> 所有数字均已核对代码：生产 10781 行 / 测试 8096 行（占比 43%）/
> 247 个测试函数 / 66 个 PG 集成测试 / 10 张业务表 / 37 次提交

---

## 一、推荐版本（中英双语，各一段）

### 中文（标准版，约 220 字）

> **DeepResearch Agent** ｜ 个人项目 ｜ Go / CloudWeGo Eino / Hertz / PostgreSQL
>
> 面向长周期研究场景的多智能体系统。核心难点不在模型调用，而在**状态语义定义、并发一致性、数据持久化**三个维度。
>
> - **状态机设计**：将"本轮结束"与"研究结束"显式分离，解决"派发子任务后模型无输出导致 Supervisor 退出、结果无人汇总"的隐蔽缺陷；配套实现结果抢占与全部终结两种唤醒机制。
> - **并发正确性**：撤销采用"先标记后拦截"，使撤销与提交的竞争窗口有确定优先级；WorkPool 改为工厂模式，每个子任务独立 Agent + 独立 ctx，实现真并行与单点取消。
> - **数据建模**：以"是否按元素增删"为单一标准决定拆表或集合类型，实现按子任务精确撤销证据；区分研究资产（归 Task）与执行产物（归 RunTask）的双粒度归属。
> - **工程质量**：建立存储契约测试，同一套断言锁死内存与 PostgreSQL 两套实现；实现 6 层 × 4 级错误分层与幂等 Wrap，45 处静默错误降至 32 处纯通知类。
>
> 10781 行生产代码 + 8096 行测试（占比 43%），247 个测试函数全绿。

### 英文（约 90 words）

> **DeepResearch Agent** — Multi-agent research system in Go (CloudWeGo Eino, Hertz, PostgreSQL).
> The hard part was not LLM calls but **state semantics, concurrency correctness, and data modeling**.
>
> - **State machine**: Explicitly separated "turn ended" from "research finished", fixing a subtle bug where the supervisor exits while sub-tasks are still pending and their results are never aggregated. Added both result-preemption and all-settled wakeups.
> - **Concurrency**: "Mark-then-block" ordering gives undo/commit races a deterministic priority; factory-based WorkPool gives each sub-task an isolated agent and context for real parallelism and targeted cancellation.
> - **Data modeling**: A single criterion (element-level mutation vs whole-value read) decides table-per-entity vs JSONB/TEXT[], enabling precise per-assignment result revocation; dual-granularity ownership separates research assets (Task) from execution artifacts (RunTask).
> - **Engineering rigor**: Storage conformance tests lock memory and PostgreSQL implementations to identical semantics; 6-layer × 4-severity error model with idempotent wrapping.
>
> 10.8k LOC production + 8.1k LOC tests (43% test ratio), 247 test functions, all passing.

---

## 二、精简版（简历空间紧张时用，3 条）

> **DeepResearch Agent** ｜ Go / CloudWeGo Eino / PostgreSQL
>
> - 设计并实现面向长周期研究的多智能体系统，攻克长期运行 Agent 的状态语义难题：显式区分"本轮结束"与"研究结束"，解决派发子任务后 Supervisor 提前退出、结果无人汇总的隐蔽缺陷。
> - 解决并发一致性：以"先标记后拦截"定义撤销与提交的竞争语义；WorkPool 采用工厂模式，每个子任务独立 Agent 与 Context，实现真并行与单点取消。
> - 完成 PostgreSQL 持久化与工程化：以统一标准决定表结构拆分，支持按子任务精确撤销；建立存储契约测试保证内存与 PG 实现行为一致。10781 行生产代码 + 8096 行测试（43%）。

---

## 三、极简版（项目列表，2 行）

> **DeepResearch Agent**（个人项目）· Go / Eino / PostgreSQL
> 长周期多智能体研究系统：解决 Agent 状态语义、撤销并发一致性、双实现数据建模问题；10.8k 生产代码 + 8.1k 测试（43%），247 个测试全绿。

---

## 四、JD 定向改写

不同岗位关注的侧重点不同，同一项目可以调整措辞重心：

### 4.1 投「后端 / 服务端开发」

把重心放在**并发、一致性、持久化**：

> **DeepResearch Agent** ｜ Go / PostgreSQL / Hertz
>
> - 设计三级流水线（调度 / 执行池 / 结果聚合）实现子任务并发与两级背压控制，`MaxConcurrency` 限制在途任务、`Workers` 限制实际并行数，避免批量派发打满资源。
> - 解决撤销与提交的竞争窗口问题：采用"先标记后拦截"顺序语义，保证用户撤销后不会有并发提交穿透；配合结果槽位墓碑机制防止槽位被重复占用。
> - 完成 PostgreSQL 持久化：10 张表建模、Commit/Revoke 全事务化、`DELETE...RETURNING` 一条语句完成撤销与审计留痕；采用行级悲观锁，8 并发提交验证无丢失更新。
> - 建立存储契约测试（同一套断言覆盖内存与 PG 两实现）与 66 个 PG 集成测试，保证换存储不改变行为。

### 4.2 投「AI / Agent / LLM 应用」

把重心放在**Agent 架构、工具治理、中断恢复**：

> **DeepResearch Agent** ｜ Go / CloudWeGo Eino
>
> - 基于 Eino `TurnLoop` 而非一次性 `Runner` 构建长生命周期 Agent，支持多轮推进、结果抢占唤醒与子任务全部终结唤醒。
> - 设计双层 Agent 架构：Supervisor 负责拆解与汇总（ReAct + 工具治理），SubAgent 独立执行子任务；子 Agent 通过工具白名单隔离控制类能力，且不注入审批中间件。
> - 实现可中断与恢复：用户消息以最高优先级抢占当前轮，被打断的原文进入上下文供模型续写；`finish_research` 不可逆且级联取消全部子任务。
> - 模型层实现可重试错误的固定间隔重试（仅限限流/不可用），配置保存前做连通性探测，避免"保存成功但一用就报错"。

### 4.3 投「基础架构 / 平台」

把重心放在**错误体系、可观测性、契约测试**：

> **DeepResearch Agent** ｜ Go
>
> - 建立 6 层 × 4 级错误模型（层级回答"谁处理"，严重度回答"是否打扰用户"），以幂等 Wrap 保证最早知情的层决定错误分类，避免跨层传递时被外层误分类。
> - 实现错误上报器：同类错误按 session + task + code + source 维度在 5 秒窗口内合并计数，避免高频失败刷屏；API 层统一错误映射，未包装错误一律脱敏为 `internal error`。
> - 完成全项目错误审计与治理：45 处静默丢弃降至 32 处纯通知类，每条静默都需能回答"失败后是否需要改变业务状态"。
> - 建立存储契约测试框架：同一套断言驱动多套实现，`Capabilities` 显式声明已知差异，将"实现漂移"变成编译期外的自动检测。

---

## 五、面试开场自述（配合简历使用，约 200 字）

> 这个项目我想做一个能真正"做研究"的 Agent，而不是一问一答。
> 这就意味着一次研究要跑几十轮、并行多个子任务、随时可能被用户打断。
>
> 做下来发现，真正的难点不是调模型，而是**在一群并发子任务随时可能被打断的时候，"什么算结束"这件事该怎么定义**。
> 我最后把它拆成了"本轮结束"和"研究结束"两种状态——前者只回到空闲等唤醒，后者才进入终态。
> 光这样还不够，因为如果所有子任务都失败，就没有"结果回来"能唤醒它，所以我又加了"全部终结唤醒"。
>
> 类似的判断还有几个：撤销和提交同时发生时谁优先、研究状态该拆表还是存 JSON、
> 内存和数据库两套实现怎么保证行为一致。这些都没有标准答案，需要回到业务语义去想。

---

## 六、写作原则说明

这段简历的写法有几条刻意的取舍，供你后续自己改稿时参考：

**1. 不写技术栈清单，写"解决了什么问题"**

对比：

- ❌「使用 Eino 框架实现多智能体协调，支持 Tools 与 SSE 流式输出」
- ✅「显式区分本轮结束与研究结束，解决派发子任务后 Supervisor 提前退出的隐蔽缺陷」

前者任何用过框架的人都能写；后者只有真正做过才知道这里有坑。

**2. 每个要点都带"为什么"，而不只是"做了什么"**

「先标记后拦截」后面跟的是「使撤销与提交的竞争窗口有确定优先级」——
说明这不是随手写的代码，而是思考过顺序语义。

**3. 用具体数字，但只留可信的**

247 个测试、43% 测试占比、10 张表、45→32 处静默错误。
不放"性能提升 300%"这类无法验证的数字。

**4. 主动暴露技术判断，而不是堆术语**

「以'是否按元素增删'为单一标准决定拆表」——这句话本身就是一个可讨论的观点，
面试官容易接着问，也容易留下印象。

**5. 不吹"高并发""海量数据"**

项目没有这类场景，写了反而危险。改为强调**正确性可验证**：
契约测试、并发测试、与竞态检测。

---

## 七、可以备查的细节（面试被追问时的数字）

| 项 | 数值 |
|---|---|
| 生产代码 | 10781 行 |
| 测试代码 | 8096 行（占比 43%） |
| 测试函数 | 247 个 |
| internal 包 | 17 个 |
| PostgreSQL 集成测试 | 66 个 |
| 契约测试 | 10 个子测试 × 2 套实现 |
| 业务表 | 10 张（checkpoints 按设计未启用） |
| 错误分层 | 6 层 × 4 级 |
| 静默错误治理 | 45 处 → 32 处（全部为通知类） |
| Git 提交 | 37 次 |
