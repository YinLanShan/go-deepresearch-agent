# 数据模型设计（MySQL 持久化）

本文件定义持久化层的数据模型，是**实现前的契约**：表结构、字段映射、
JSON 与拆表的边界、事务与并发约定。

- 目标数据库：MySQL 8.0+
- 字符集：`utf8mb4` / `utf8mb4_0900_ai_ci`（支持 emoji 与多语言）
- 引擎：InnoDB（需要事务与外键）

---

## 0. 总体原则

| 原则 | 说明 |
|---|---|
| **内存实现保留** | 作为参考实现与测试替身，180 个测试仍跑内存 |
| **ID 由业务层生成** | 沿用 `ids.New()` 的 32 位 hex 字符串，表用 `CHAR(32)` |
| **硬删除 + 外键级联** | 与当前内存实现的语义一致，不做软删除 |
| **每个 Store 方法一个事务** | 跨 Store 的级联由上层（Runtime）协调 |
| **时间统一 UTC** | `DATETIME(3)` 毫秒精度，应用层负责时区转换 |
| **JSON 列用于"整体读写"的集合** | 需要按字段增删的集合才拆表 |

### JSON 与拆表的判断标准

> **需要按元素级增删改 → 拆表；只需整体读写 → JSON。**

据此：

| 集合 | 现状需求 | 决定 |
|---|---|---|
| `Results` | **按 AssignmentID 精确删除**（撤销功能） | **拆表** |
| `Findings` | 只追加，整体读 | 拆表（与 Results 同生命周期，便于 JOIN 与分页） |
| `CommitLog` | 只追加，按序读 | **拆表**（审计需要按 op / 时间查询） |
| `History` | 只追加，可能很长，前端分页 | **拆表** |
| `Requirements` | 只追加，整体读 | JSON |
| `RelatedNodes` | 创建时写入，之后只读 | JSON |
| `Conclusion` / `InterruptedContext` | 标量 | 普通列 |

---

## 1. 表结构总览

```
sessions
  └── tasks                      (FK: session_id)
        └── run_tasks            (FK: task_id, UNIQUE)
        └── research_states      (FK: task_id, UNIQUE)
              ├── research_results      (FK: state_id)
              ├── research_findings     (FK: state_id)
              ├── research_commit_log   (FK: state_id)
              └── research_messages     (FK: state_id)
        └── assignments          (FK: run_task_id)
              └── works          (FK: assignment_id)
```

**归属关系（与业务语义一致）：**

| 实体 | 归属 | 级联行为 |
|---|---|---|
| Task | Session | Session 删除 → Task 删除 |
| RunTask | Task | Task 删除 → RunTask 删除 |
| ResearchState | **Task**（跨 RunTask 存活） | Task 删除 → 删除 |
| Assignment | **RunTask** | RunTask 删除 → 删除 |
| Work | **RunTask**（经 Assignment） | RunTask 删除 → 删除 |

> 注意区分：ResearchState 归属 **Task**，Assignment/Work 归属 **RunTask**。
> 这是之前明确的设计，不要统一成同一个粒度。

---

## 2. 建表 SQL

### 2.1 sessions

```sql
CREATE TABLE sessions (
  id         CHAR(32)     NOT NULL,
  title      VARCHAR(255) NOT NULL DEFAULT '',
  created_at DATETIME(3)  NOT NULL,
  updated_at DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

### 2.2 tasks

```sql
CREATE TABLE tasks (
  id          CHAR(32)     NOT NULL,
  session_id  CHAR(32)     NOT NULL,
  user_id     VARCHAR(64)  NOT NULL DEFAULT '',
  title       VARCHAR(255) NOT NULL DEFAULT '',
  description TEXT         NULL,
  enabled     TINYINT(1)   NOT NULL DEFAULT 0,
  created_at  DATETIME(3)  NOT NULL,
  updated_at  DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_session (session_id, created_at),
  CONSTRAINT fk_tasks_session FOREIGN KEY (session_id)
    REFERENCES sessions (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`enabled` 用于"Task 是否允许启动新的执行"，与 RunTask 状态相互独立。

### 2.3 run_tasks

```sql
CREATE TABLE run_tasks (
  id            CHAR(32)    NOT NULL,
  task_id       CHAR(32)    NOT NULL,
  status        VARCHAR(16) NOT NULL,
  turn          INT         NOT NULL DEFAULT 0,
  state_version BIGINT      NOT NULL DEFAULT 0,
  created_at    DATETIME(3) NOT NULL,
  updated_at    DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  -- 一个 Task 同时只有一个 RunTask（重建会替换）
  UNIQUE KEY uk_task (task_id),
  KEY idx_status (status),
  CONSTRAINT fk_runtasks_task FOREIGN KEY (task_id)
    REFERENCES tasks (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`status` 取值：`created` / `running` / `waiting` / `stopped` / `finished`。

### 2.4 research_states

```sql
CREATE TABLE research_states (
  id                  CHAR(32)     NOT NULL,
  task_id             CHAR(32)     NOT NULL,
  session_id          CHAR(32)     NOT NULL,
  ordinal             INT          NOT NULL DEFAULT 1,

  -- 标量
  conclusion          MEDIUMTEXT   NULL,
  interrupted_context MEDIUMTEXT   NULL,

  -- 整体读写的集合（只追加，不按元素增删）
  requirements        JSON         NULL,   -- string[]
  related_nodes       JSON         NULL,   -- RelatedNode[]

  version             BIGINT       NOT NULL DEFAULT 0,
  created_at          DATETIME(3)  NOT NULL,
  updated_at          DATETIME(3)  NOT NULL,

  PRIMARY KEY (id),
  -- 一个 Task 一份 ResearchState
  UNIQUE KEY uk_task (task_id),
  KEY idx_session_ordinal (session_id, ordinal),
  CONSTRAINT fk_states_task FOREIGN KEY (task_id)
    REFERENCES tasks (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

**为什么 `requirements` / `related_nodes` 用 JSON：**

- `requirements`：只追加、整体读进提示词，无需按元素查询；
- `related_nodes`：Task 创建时一次性写入的快照，之后只读。

**为什么不把 `results` 放这里：** 见 2.5。

### 2.5 research_results

```sql
CREATE TABLE research_results (
  id            CHAR(32)    NOT NULL,
  state_id      CHAR(32)    NOT NULL,
  assignment_id CHAR(32)    NOT NULL DEFAULT '',
  turn          INT         NOT NULL,
  status        VARCHAR(16) NOT NULL,
  content       MEDIUMTEXT  NOT NULL,
  created_at    DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  -- 撤销功能：按 assignment_id 精确删除
  KEY idx_state_assignment (state_id, assignment_id),
  KEY idx_state_turn (state_id, turn),
  CONSTRAINT fk_results_state FOREIGN KEY (state_id)
    REFERENCES research_states (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

**关键：`assignment_id` 必须有索引。**

撤销流程 `RevokeResult(taskID, assignmentID, reason)` 在这里就是：

```sql
DELETE FROM research_results
WHERE state_id = (SELECT id FROM research_states WHERE task_id = ?)
  AND assignment_id = ?;
```

### 2.6 research_findings

```sql
CREATE TABLE research_findings (
  id         CHAR(32)    NOT NULL,
  state_id   CHAR(32)    NOT NULL,
  turn       INT         NOT NULL,
  content    MEDIUMTEXT  NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_state_turn (state_id, turn),
  CONSTRAINT fk_findings_state FOREIGN KEY (state_id)
    REFERENCES research_states (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

### 2.7 research_commit_log

```sql
CREATE TABLE research_commit_log (
  seq           BIGINT      NOT NULL AUTO_INCREMENT,
  state_id      CHAR(32)    NOT NULL,
  op            VARCHAR(32) NOT NULL,
  turn          INT         NOT NULL DEFAULT 0,
  assignment_id CHAR(32)    NOT NULL DEFAULT '',
  result_id     CHAR(32)    NOT NULL DEFAULT '',
  content       MEDIUMTEXT  NULL,
  reason        VARCHAR(512) NOT NULL DEFAULT '',
  version       BIGINT      NOT NULL,
  created_at    DATETIME(3) NOT NULL,
  PRIMARY KEY (seq),
  KEY idx_state (state_id, seq),
  KEY idx_op_time (op, created_at),
  CONSTRAINT fk_commitlog_state FOREIGN KEY (state_id)
    REFERENCES research_states (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`op` 取值：`commit_result` / `commit_finding` / `revoke_result` / `set_conclusion`。

**注意：`seq` 是全局自增，不再使用业务层的序号。**
业务层的 `CommitRecord.Seq` 读出来后按 `state_id` 内部顺序重新编号。

### 2.8 research_messages

```sql
CREATE TABLE research_messages (
  seq        BIGINT      NOT NULL AUTO_INCREMENT,
  state_id   CHAR(32)    NOT NULL,
  role       VARCHAR(16) NOT NULL,
  content    MEDIUMTEXT  NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (seq),
  KEY idx_state (state_id, seq),
  CONSTRAINT fk_messages_state FOREIGN KEY (state_id)
    REFERENCES research_states (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`role` 取值：`user` / `assistant`。

### 2.9 assignments

```sql
CREATE TABLE assignments (
  id          CHAR(32)     NOT NULL,
  session_id  CHAR(32)     NOT NULL,
  task_id     CHAR(32)     NOT NULL,
  run_task_id CHAR(32)     NOT NULL,
  goal        TEXT         NOT NULL,
  status      VARCHAR(24)  NOT NULL,
  attempt     INT          NOT NULL DEFAULT 0,
  result      MEDIUMTEXT   NULL,
  err         TEXT         NULL,
  created_at  DATETIME(3)  NOT NULL,
  updated_at  DATETIME(3)  NOT NULL,
  finished_at DATETIME(3)  NULL,
  PRIMARY KEY (id),
  KEY idx_run_task (run_task_id, created_at),
  KEY idx_status (status),
  CONSTRAINT fk_assignments_runtask FOREIGN KEY (run_task_id)
    REFERENCES run_tasks (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`status` 取值：`pending` / `scheduled` / `running` / `awaiting_result` /
`completed` / `failed` / `cancelled`。

### 2.10 works

```sql
CREATE TABLE works (
  id            CHAR(32)     NOT NULL,
  assignment_id CHAR(32)     NOT NULL,
  session_id    CHAR(32)     NOT NULL,
  task_id       CHAR(32)     NOT NULL,
  run_task_id   CHAR(32)     NOT NULL,
  goal          TEXT         NOT NULL,
  status        VARCHAR(16)  NOT NULL,
  constraint_json JSON       NULL,   -- WorkConstraint，当前仅预留
  worker_id     VARCHAR(64)  NOT NULL DEFAULT '',
  attempt       INT          NOT NULL DEFAULT 1,
  stage         VARCHAR(32)  NOT NULL DEFAULT '',
  detail        VARCHAR(512) NOT NULL DEFAULT '',
  result        MEDIUMTEXT   NULL,
  err           TEXT         NULL,
  started_at    DATETIME(3)  NULL,
  finished_at   DATETIME(3)  NULL,
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_run_task (run_task_id, created_at),
  KEY idx_assignment (assignment_id),
  CONSTRAINT fk_works_assignment FOREIGN KEY (assignment_id)
    REFERENCES assignments (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

`status` 取值：`pending` / `running` / `succeeded` / `failed` /
`cancelled` / `timedout`。

### 2.11 checkpoints（可选）

```sql
CREATE TABLE checkpoints (
  checkpoint_id VARCHAR(128) NOT NULL,
  data          LONGBLOB     NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (checkpoint_id),
  KEY idx_updated (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

**建议不放 MySQL**，理由：

- Eino checkpoint 是 gob 编码，可能几 MB；
- 生命周期极短（中断 → 恢复即删除）；
- 高频读写会拖慢主库。

**替代方案**：Redis（带 TTL）或本地文件。若必须用 MySQL，
请使用 `LONGBLOB` 并配合定期清理任务。

---

## 3. 字段映射

### 3.1 ResearchState 拆分关系

```
research_states (1 行)
├── requirement_list   → requirements JSON
├── related_nodes      → related_nodes JSON
├── conclusion         → conclusion 列
├── interrupted_context→ interrupted_context 列
├── results[]          → research_results 表（N 行）
├── findings[]         → research_findings 表（N 行）
├── commit_log[]       → research_commit_log 表（N 行）
└── history[]          → research_messages 表（N 行）
```

**读取 `Get(taskID)` 需要 4 次额外查询**（results/findings/commitlog/messages）。

优化建议：

- `Get` 常用于构建提示词，需要全部数据 → 一次 4 表 JOIN 或 4 次查询均可；
- 前端列表页只需要标量 → 单独提供 `GetMeta`。

### 3.2 时间字段

| Go | MySQL | 说明 |
|---|---|---|
| `time.Time`（必填） | `DATETIME(3) NOT NULL` | createdAt / updatedAt |
| `time.Time`（可空） | `DATETIME(3) NULL` | finishedAt / startedAt |
| 零值 `time.Time{}` | `NULL` | 不要存 `0001-01-01`，那是 Go 零值的误存 |

> **重要**：当前内存实现里 `FinishedAt` 零值会序列化成
> `"0001-01-01T00:00:00Z"`。MySQL 实现应存 `NULL`，
> 读取时再转回零值，避免前端显示异常时间。

### 3.3 枚举字段

全部用 `VARCHAR` 而非 `ENUM`：

- 便于新增状态而不改表结构；
- 应用层已有 `IsTerminal()` 等判断；
- 加索引后查询性能足够。

**读取时需校验**：非法值应返回错误而不是静默接受。

---

## 4. 并发与事务

### 4.1 Commit 的乐观锁

`Commit` / `RevokeResult` / `SetConclusion` 都需要改 `version`：

```sql
UPDATE research_states
SET version = version + 1, updated_at = ?
WHERE id = ? AND version = ?;
```

影响行数为 0 → 版本冲突。

**当前策略：记录日志但不重试。**

理由：Commit 由 ResultAggregator 的**单 goroutine 串行**执行，
实际冲突概率极低。等真出现并发再加重试。

### 4.2 事务边界

| 操作 | 事务范围 |
|---|---|
| `Commit` | research_states 的 version + results 插入 + findings 插入 + commit_log 插入 = **一个事务** |
| `RevokeResult` | results 删除 + commit_log 插入 + version 自增 = **一个事务** |
| `DeleteByRunTask` | assignments + works 删除 = **一个事务** |
| Session 级联删除 | 由 Runtime 逐层调用，每层各自事务（与当前协商流程一致） |

### 4.3 不该放进事务的

- **事件发布**：`bus.Publish` 在事务提交后调用，避免事务回滚但事件已发出；
- **给前端推送 SSE**：同上。

### 4.4 提交门控（StopCommits / ResumeCommits）

当前语义是"派生状态、自愈、不返回 error"。

**MySQL 实现建议：存在 `run_tasks` 表的一个列？** 不——门控针对的是
ResearchState 的 Commit 能力，但它的生命周期与 RunTask 绑定。

**建议做法**：内存态 + 启动时重建。

- 门控是纯运行时状态，进程重启后所有 RunTask 要么已终止、要么需恢复；
- 持久化它反而要处理"重启后门控该默认开还是关"的歧义；
- 因此：**不落库**，`StopCommits`/`ResumeCommits` 仍操作内存 map，
  Store 构造时按 RunTask 状态初始化（终态 → 关闭，否则 → 开启）。

---

## 5. 索引设计理由

| 索引 | 支撑的查询 |
|---|---|
| `tasks(session_id, created_at)` | 列出某 Session 的全部 Task（前端切换会话） |
| `run_tasks(status)` | 全局单例运行检查（`disableOtherRunningSessions`） |
| `research_states(session_id, ordinal)` | `GetLatestBySession` / `ListBySession` |
| `research_results(state_id, assignment_id)` | **撤销时精确定位** |
| `research_results(state_id, turn)` | 按轮次展示 |
| `research_commit_log(op, created_at)` | 审计查询（"最近所有撤销"） |
| `assignments(run_task_id, created_at)` | 后台任务面板 |
| `assignments(status)` | 查未完成 Assignment（`HasPendingAssignments`） |
| `works(run_task_id, created_at)` | 后台任务面板 |
| `works(assignment_id)` | 查某 Assignment 的执行实例 |

---

## 6. 实现顺序建议

| 阶段 | 内容 | 依赖 |
|---|---|---|
| **1** | 建表脚本 + Session/Task/RunTask 三个存储 | 无 |
| **2** | Assignment / Work 存储（级联删除） | 阶段 1 |
| **3** | ResearchState 标量部分 + Conclusion | 阶段 1 |
| **4** | results / findings / commitlog / messages 四张子表 | 阶段 3 |
| **5** | 撤销与 Commit 的乐观锁 | 阶段 4 |
| **6** | 双实现并行 + 一致性测试 | 全部 |
| **7** | Checkpoint（可选，建议先不做） | —— |

**每个阶段都应能独立跑通测试**：内存实现保持不变，
MySQL 实现用同一批测试用例验证（通过 Store 接口抽象）。

---

## 7. 待确认的决策

| # | 问题 | 建议 |
|---|---|---|
| D1 | `History` 是否需要分页接口？ | 当前 `Get` 全量读；超过 1000 条再考虑 |
| D2 | `ResearchState.Get` 是否拆成 `GetMeta` + `GetFull`？ | 先不拆，等前端列表页有性能问题再说 |
| D3 | 是否需要 `research_results.status`（pending/committed）？ | 需要，当前 `Result.Pending` 语义要保留 |
| D4 | 乐观锁冲突如何处理？ | 记日志，不重试（等真并发再议） |
| D5 | Checkpoint 是否入 MySQL？ | **建议不入**，用 Redis 或文件 |
| D6 | 是否需要数据迁移脚本（从内存导出）？ | 当前无生产数据，不需要 |

---

## 8. 与内存实现的语义对齐清单

实现 MySQL 版时，以下行为**必须与内存实现一致**（已有测试覆盖）：

- [ ] `Create` 主键冲突 → 返回错误（`storage.ErrAlreadyExists`，见下方说明）
- [ ] `Update` 目标不存在 → 返回 `ErrNotFound`
- [ ] `Delete` 目标不存在 → 返回 `ErrNotFound`（除级联删除外幂等）
- [ ] `ListByRunTask` 返回**按插入顺序**（内存实现用 `seq` 切片维持插入序；
      MySQL 版须用 `created_at` 升序 + 主键兜底，避免同毫秒插入时顺序不稳）
- [ ] `Get` 返回的对象**修改后不影响存储**（内存实现靠深拷贝，MySQL 天然满足）
- [ ] `RevokeResult` 返回被撤销的条数
- [ ] `Commit` 在门控关闭时返回 `researchstate.ErrNotAccepting`
- [ ] `SetConclusion` 会写 `commit_log`
- [ ] 空 `time.Time{}` 读写往返后仍是零值

### ⚠️ 已知不一致（需在持久化前统一）

内存实现的 `Create` 冲突**没有使用哨兵错误**：

```go
// storage/memory/memory.go
return fmt.Errorf("storage: assignment %q already exists", a.ID)   // ← 无法 errors.Is
return fmt.Errorf("storage: work %q already exists", w.ID)
```

而 `storage` 包里其实定义了 `ErrRunTaskExists` 却没有类似的 `ErrAlreadyExists`。

**建议先统一为哨兵错误**：新增 `storage.ErrAlreadyExists`，
三个 Store 的 `Create` 冲突都用它包装。否则 MySQL 版无法复用同一套
`errors.Is` 判断，调用方只能比对字符串（违反错误手册"不依赖字符串"的约定）。

这件事应在阶段 1 之前完成，改动很小但影响接口一致性。

---

## 9. 配置项（建议）

```
DRA_DB_ENABLED     = false          // 默认仍用内存，便于本地开发
DRA_DB_DSN         = "user:pass@tcp(127.0.0.1:3306)/dra?parseTime=true&loc=UTC&charset=utf8mb4"
DRA_DB_MAX_OPEN    = 20
DRA_DB_MAX_IDLE    = 10
DRA_DB_CONN_MAX_LIFETIME = 1800     // 秒
```

`parseTime=true` 必须开启，否则 `DATETIME` 无法扫描进 `time.Time`。
