# 数据模型设计（PostgreSQL 持久化）

本文件定义持久化层的数据模型，是**实现前的契约**：表结构、字段映射、
JSON 与拆表的边界、事务与并发约定。

- 目标数据库：**PostgreSQL 14+**（本地验证版本 18.3）
- 驱动：`github.com/jackc/pgx/v5/stdlib`（标准 `database/sql` 接口 + pgx 内核）
- 编码：`UTF8`（数据库级）
- 时区：`TIMESTAMPTZ`，由数据库负责时区语义

---

## 0. 总体原则

| 原则 | 说明 |
|---|---|
| **内存实现保留** | 作为参考实现与测试替身，180 个测试仍跑内存 |
| **ID 由业务层生成** | 沿用 `ids.New()` 的 32 位 hex 字符串，表用 `TEXT` |
| **硬删除 + 外键级联** | 与当前内存实现的语义一致，不做软删除 |
| **每个 Store 方法一个事务** | 跨 Store 的级联由上层（Runtime）协调 |
| **时间统一 `TIMESTAMPTZ`** | 由数据库处理时区，应用层无需手工换算 |
| **JSON 列用于"整体读写"的集合** | 需要按字段增删的集合才拆表 |
| **原生类型优先** | `TEXT[]` 而非 JSON 存字符串数组；`BOOLEAN` 而非 0/1 |

### JSON 与拆表的判断标准

> **需要按元素级增删改 → 拆表；只需整体读写 → 集合类型列。**

据此：

| 集合 | 现状需求 | 决定 |
|---|---|---|
| `Results` | **按 AssignmentID 精确删除**（撤销功能） | **拆表** |
| `Findings` | 只追加，整体读 | 拆表（与 Results 同生命周期） |
| `CommitLog` | 只追加，按序读 | **拆表**（审计需按 op / 时间查询） |
| `History` | 只追加，可能很长，前端分页 | **拆表** |
| `Requirements` | 只追加，整体读，`[]string` | **`TEXT[]`** |
| `RelatedNodes` | 创建时写入，之后只读，嵌套结构 | `JSONB` |
| `Conclusion` / `InterruptedContext` | 标量 | 普通列 |

**为什么 `Requirements` 用 `TEXT[]` 而不是 `JSONB`：**

它是纯粹的字符串数组、无嵌套，用原生数组可：

```sql
-- 按关键词检索需求（JSON 做不到这么直接）
SELECT * FROM research_states
WHERE EXISTS (SELECT 1 FROM unnest(requirements) r WHERE r ILIKE '%Eino%');

-- 统计每个节点的需求条数
SELECT task_id, cardinality(requirements) FROM research_states;
```

**为什么 `RelatedNodes` 用 `JSONB`：**

它是嵌套结构（每个节点含 requirements / results / findings / conclusion），
拆表会产生 4 层嵌套关系，收益远低于复杂度。`JSONB` 还能建 GIN 索引备用。

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

## 2. 建表 SQL（PostgreSQL）

### 2.1 sessions

```sql
CREATE TABLE sessions (
  id         TEXT    PRIMARY KEY CHECK (length(id) = 32),
  title      TEXT        NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_sessions_created ON sessions (created_at);
```

### 2.2 tasks

```sql
CREATE TABLE tasks (
  id          TEXT    PRIMARY KEY CHECK (length(id) = 32),
  session_id  TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  user_id     TEXT        NOT NULL DEFAULT '',
  title       TEXT        NOT NULL DEFAULT '',
  description TEXT,
  enabled     BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_tasks_session ON tasks (session_id, created_at);
```

`enabled` 用于"Task 是否允许启动新的执行"，与 RunTask 状态相互独立。

### 2.3 run_tasks

```sql
CREATE TABLE run_tasks (
  id            TEXT    PRIMARY KEY CHECK (length(id) = 32),
  task_id       TEXT    NOT NULL UNIQUE REFERENCES tasks (id) ON DELETE CASCADE,
  status        TEXT        NOT NULL,
  turn          INTEGER     NOT NULL DEFAULT 0,
  state_version BIGINT      NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_run_tasks_status ON run_tasks (status);

-- 只索引"活跃"运行，加速全局单例检查
CREATE INDEX idx_run_tasks_active ON run_tasks (status)
  WHERE status IN ('running', 'waiting');
```

`status` 取值：`created` / `running` / `waiting` / `stopped` / `finished`。

**部分索引**：PostgreSQL 支持 `WHERE` 子句，只索引活跃记录，
比 MySQL 需要全表索引更省空间。

### 2.4 research_states

```sql
CREATE TABLE research_states (
  id                  TEXT    PRIMARY KEY CHECK (length(id) = 32),
  task_id             TEXT    NOT NULL UNIQUE REFERENCES tasks (id) ON DELETE CASCADE,
  session_id          TEXT    NOT NULL,
  ordinal             INTEGER     NOT NULL DEFAULT 1,

  -- 标量
  conclusion          TEXT,
  interrupted_context TEXT,

  -- 整体读写的集合
  requirements        TEXT[]      NOT NULL DEFAULT '{}',   -- string[]
  related_nodes       JSONB       NOT NULL DEFAULT '[]',   -- RelatedNode[]

  version             BIGINT      NOT NULL DEFAULT 0,
  created_at          TIMESTAMPTZ NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_states_session ON research_states (session_id, ordinal);

-- 需要按需求内容检索时启用
CREATE INDEX idx_states_requirements ON research_states USING GIN (requirements);
CREATE INDEX idx_states_related ON research_states USING GIN (related_nodes);
```

**`TEXT[]` 与 `JSONB` 都配了 GIN 索引**，当前不用，但未来加检索时无需改表。

### 2.5 research_results

```sql
CREATE TABLE research_results (
  id            TEXT    PRIMARY KEY CHECK (length(id) = 32),
  state_id      TEXT    NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  assignment_id TEXT    NOT NULL DEFAULT '',
  turn          INTEGER     NOT NULL,
  status        TEXT        NOT NULL,
  content       TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_results_state_assignment ON research_results (state_id, assignment_id);
CREATE INDEX idx_results_state_turn ON research_results (state_id, turn);
```

**关键：`(state_id, assignment_id)` 索引支撑撤销。**

撤销流程 `RevokeResult(taskID, assignmentID, reason)`：

```sql
DELETE FROM research_results
WHERE state_id = (SELECT id FROM research_states WHERE task_id = $1)
  AND assignment_id = $2
RETURNING id, assignment_id, turn, content;   -- ← PG 的 RETURNING 直接拿到被删行
```

**`RETURNING` 是 PostgreSQL 的关键优势**：撤销需要把被删内容写进
`commit_log`，一条语句即可完成，无需先查后删。

### 2.6 research_findings

```sql
CREATE TABLE research_findings (
  id         TEXT    PRIMARY KEY CHECK (length(id) = 32),
  state_id   TEXT    NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  turn       INTEGER     NOT NULL,
  content    TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_findings_state_turn ON research_findings (state_id, turn);
```

### 2.7 research_commit_log

```sql
CREATE TABLE research_commit_log (
  seq           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  state_id      TEXT    NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  op            TEXT        NOT NULL,
  turn          INTEGER     NOT NULL DEFAULT 0,
  assignment_id TEXT    NOT NULL DEFAULT '',
  result_id     TEXT    NOT NULL DEFAULT '',
  content       TEXT,
  reason        TEXT        NOT NULL DEFAULT '',
  version       BIGINT      NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_commitlog_state ON research_commit_log (state_id, seq);
CREATE INDEX idx_commitlog_op ON research_commit_log (op, created_at DESC);
```

`op` 取值：`commit_result` / `commit_finding` / `revoke_result` / `set_conclusion`。

**`seq` 使用 `GENERATED ALWAYS AS IDENTITY`**（SQL 标准，优于 `BIGSERIAL`）。
业务层的 `CommitRecord.Seq` 读出后按 `state_id` 内部顺序重新编号。

### 2.8 research_messages

```sql
CREATE TABLE research_messages (
  seq        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  state_id   TEXT    NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  role       TEXT        NOT NULL,
  content    TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_messages_state ON research_messages (state_id, seq);
```

`role` 取值：`user` / `assistant`。

### 2.9 assignments

```sql
CREATE TABLE assignments (
  id          TEXT    PRIMARY KEY CHECK (length(id) = 32),
  session_id  TEXT    NOT NULL,
  task_id     TEXT    NOT NULL,
  run_task_id TEXT    NOT NULL REFERENCES run_tasks (id) ON DELETE CASCADE,
  goal        TEXT        NOT NULL,
  status      TEXT        NOT NULL,
  attempt     INTEGER     NOT NULL DEFAULT 0,
  result      TEXT,
  err         TEXT,
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ
);

CREATE INDEX idx_assignments_runtask ON assignments (run_task_id, created_at);
CREATE INDEX idx_assignments_status ON assignments (status);

-- 加速"是否还有未完成 Assignment"的判定
CREATE INDEX idx_assignments_pending ON assignments (run_task_id)
  WHERE status NOT IN ('completed', 'failed', 'cancelled');
```

`status` 取值：`pending` / `scheduled` / `running` / `awaiting_result` /
`completed` / `failed` / `cancelled`。

**部分索引 `idx_assignments_pending`** 直接支撑 `HasPendingAssignments`
与 `maybePublishSettled` 的热路径查询。

### 2.10 works

```sql
CREATE TABLE works (
  id            TEXT    PRIMARY KEY CHECK (length(id) = 32),
  assignment_id TEXT    NOT NULL REFERENCES assignments (id) ON DELETE CASCADE,
  session_id    TEXT    NOT NULL,
  task_id       TEXT    NOT NULL,
  run_task_id   TEXT    NOT NULL,
  goal          TEXT        NOT NULL,
  status        TEXT        NOT NULL,
  constraint_data JSONB,             -- WorkConstraint，当前仅预留
  worker_id     TEXT        NOT NULL DEFAULT '',
  attempt       INTEGER     NOT NULL DEFAULT 1,
  stage         TEXT        NOT NULL DEFAULT '',
  detail        TEXT        NOT NULL DEFAULT '',
  result        TEXT,
  err           TEXT,
  started_at    TIMESTAMPTZ,
  finished_at   TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_works_runtask ON works (run_task_id, created_at);
CREATE INDEX idx_works_assignment ON works (assignment_id);
```

`status` 取值：`pending` / `running` / `succeeded` / `failed` /
`cancelled` / `timedout`。

> `constraint` 是 PostgreSQL 保留字，列名改用 `constraint_data`。

### 2.11 checkpoints（可选，建议不启用）

```sql
CREATE TABLE checkpoints (
  checkpoint_id TEXT        PRIMARY KEY,
  data          BYTEA       NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_checkpoints_updated ON checkpoints (updated_at);
```

**建议不放 PostgreSQL**，理由：

- Eino checkpoint 是 gob 编码，可能几 MB；
- 生命周期极短（中断 → 恢复即删除）；
- 高频读写会污染主库的 WAL。

**替代方案**：Redis（带 TTL）或本地文件。若必须入库，请配合定期清理。

---

## 3. 字段映射

### 3.1 ResearchState 拆分关系

```
research_states (1 行)
├── requirements       → requirements TEXT[]
├── related_nodes      → related_nodes JSONB
├── conclusion         → conclusion 列
├── interrupted_context→ interrupted_context 列
├── results[]          → research_results 表（N 行）
├── findings[]         → research_findings 表（N 行）
├── commit_log[]       → research_commit_log 表（N 行）
└── history[]          → research_messages 表（N 行）
```

**读取 `Get(taskID)` 需要 4 次额外查询。**

优化建议：

- `Get` 常用于构建提示词，需要全部数据 → 4 次查询可接受（各带索引）；
- 前端列表页只需要标量 → 后续可加 `GetMeta`。

### 3.2 时间字段

| Go | PostgreSQL | 说明 |
|---|---|---|
| `time.Time`（必填） | `TIMESTAMPTZ NOT NULL` | createdAt / updatedAt |
| `time.Time`（可空） | `TIMESTAMPTZ` | finishedAt / startedAt |
| 零值 `time.Time{}` | `NULL` | 不要存 `0001-01-01` |

> **重要**：当前内存实现里 `FinishedAt` 零值会序列化成
> `"0001-01-01T00:00:00Z"`。PG 实现应存 `NULL`，
> 读取时再转回零值，避免前端显示异常时间。

**`TIMESTAMPTZ` 的语义**：存储为 UTC，读取时返回带时区的 `time.Time`。
Go 的 `pgx` 驱动会自动处理，无需手工 `loc=UTC`。

### 3.3 枚举字段

全部用 `TEXT` 而非 PG 的 `ENUM` 类型：

- PG 的 `ENUM` 增删值需要 `ALTER TYPE`，迁移成本高；
- 应用层已有 `IsTerminal()` 等判断；
- 加索引后查询性能足够。

**可选增强**：用 `CHECK` 约束保证取值合法。

```sql
status TEXT NOT NULL
  CHECK (status IN ('pending','scheduled','running','awaiting_result',
                    'completed','failed','cancelled'))
```

我建议**加 CHECK**——PG 的约束校验开销很小，能防止脏数据。

### 3.4 Go 类型映射（pgx）

| PG 类型 | Go 类型 | 备注 |
|---|---|---|
| `TEXT` | `string` | pgx 会把 `CHAR` 补空格，**建议改用 `TEXT`**，见下 |
| `TEXT` | `string` | |
| `INTEGER` | `int` | |
| `BIGINT` | `int64` | |
| `BOOLEAN` | `bool` | |
| `TIMESTAMPTZ` | `time.Time` | |
| `TEXT[]` | `[]string` | pgx 原生支持 |
| `JSONB` | `[]byte` 或自定义类型 | 建议 `[]byte` + `encoding/json` |
| `BYTEA` | `[]byte` | |

本文件全部 ID 列使用 `TEXT` + `CHECK (length(...) = 32)`，而非 `CHAR(32)`。

**原因**：PostgreSQL 的 `CHAR(n)` 是定长、会用空格补齐。
虽然比较时忽略尾部空格，但**取出时可能带空格**，
Go 侧拿到 `"abc...  "` 会导致 map 查找失败、ID 比较不等等隐蔽问题。

`TEXT` + `CHECK` 既保证长度约束，又不会引入填充字符，
且与 MySQL 的 `VARCHAR(32)` 语义一致，便于将来迁移。

---

## 4. 并发与事务

### 4.1 Commit 的乐观锁

`Commit` / `RevokeResult` / `SetConclusion` 都需要改 `version`：

```sql
UPDATE research_states
SET version = version + 1, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING version;
```

无返回行 → 版本冲突。

**当前策略：记录日志但不重试。**

理由：Commit 由 ResultAggregator 的**单 goroutine 串行**执行，
实际冲突概率极低。等真出现并发再加重试。

### 4.2 事务边界

| 操作 | 事务范围 |
|---|---|
| `Commit` | state version + results 插入 + findings 插入 + commit_log 插入 = **一个事务** |
| `RevokeResult` | results 删除（RETURNING）+ commit_log 插入 + version 自增 = **一个事务** |
| `DeleteByRunTask` | assignments + works 删除 = **一个事务**（可由外键级联一次完成） |
| Session 级联删除 | 由 Runtime 逐层调用，每层各自事务（与当前协商流程一致） |

### 4.3 不该放进事务的

- **事件发布**：`bus.Publish` 在事务提交后调用，避免事务回滚但事件已发出；
- **给前端推送 SSE**：同上。

### 4.4 提交门控（StopCommits / ResumeCommits）

当前语义是"派生状态、自愈、不返回 error"。

**决定：不落库，保持内存态。**

- 门控是纯运行时状态，进程重启后所有 RunTask 要么已终止、要么需恢复；
- 持久化它要处理"重启后门控该默认开还是关"的歧义；
- Store 构造时按 RunTask 状态初始化（终态 → 关闭，否则 → 开启）。

### 4.5 连接池

```go
db, err := sql.Open("pgx", dsn)   // 使用 pgx/stdlib
db.SetMaxOpenConns(20)
db.SetMaxIdleConns(10)
db.SetConnMaxLifetime(30 * time.Minute)
db.SetConnMaxIdleTime(5 * time.Minute)
```

**DSN 示例：**

```
postgres://user:pass@127.0.0.1:5432/dra?sslmode=disable&application_name=dra
```

生产环境请把 `sslmode` 改为 `require` 或 `verify-full`。

---

## 5. 索引设计理由

| 索引 | 支撑的查询 |
|---|---|
| `tasks(session_id, created_at)` | 列出某 Session 的全部 Task |
| `run_tasks(status)` | 全局单例运行检查 |
| `run_tasks(status) WHERE active` | **部分索引**，只索引活跃运行 |
| `research_states(session_id, ordinal)` | `GetLatestBySession` / `ListBySession` |
| `research_results(state_id, assignment_id)` | **撤销时精确定位** |
| `research_results(state_id, turn)` | 按轮次展示 |
| `research_commit_log(op, created_at DESC)` | 审计查询（"最近所有撤销"） |
| `assignments(run_task_id, created_at)` | 后台任务面板 |
| `assignments(status) WHERE pending` | `HasPendingAssignments` 热路径 |
| `works(run_task_id, created_at)` | 后台任务面板 |
| `works(assignment_id)` | 查某 Assignment 的执行实例 |

**部分索引是 PostgreSQL 的独有优势**：只索引需要的子集，
索引体积更小、更新更快。

---

## 6. 实现顺序与进度

| 阶段 | 内容 | 依赖 | 状态 |
|---|---|---|---|
| **0** | 补 `storage.ErrAlreadyExists` / `ErrInvalidArgument` 哨兵错误（内存实现同步） | 无 | ✅ 已完成 |
| **1** | 建表脚本 + Session/Task/RunTask 三个存储 | 阶段 0 | ✅ 已完成 |
| **2** | Assignment / Work 存储（级联删除） | 阶段 1 | ⬜ 待做 |
| **3** | ResearchState 标量部分 + Conclusion | 阶段 1 | ⬜ 待做 |
| **4** | results / findings / commitlog / messages 四张子表 | 阶段 3 | ⬜ 待做 |
| **5** | 撤销与 Commit 的乐观锁 | 阶段 4 | ⬜ 待做 |
| **6** | 双实现并行 + 一致性测试 | 全部 | ⬜ 待做 |
| **7** | Checkpoint（可选，建议先不做） | —— | ⬜ 待做 |

**每个阶段都应能独立跑通测试**：内存实现保持不变，
PG 实现用同一批测试用例验证（通过 Store 接口抽象）。

### 阶段 1 已落地的内容

| 文件 | 内容 |
|---|---|
| `internal/storage/postgres/migrations/0001_init.sql` | 11 张表完整建表脚本（embed 进二进制） |
| `internal/storage/postgres/postgres.go` | 连接池、`Migrate`（幂等执行脚本） |
| `internal/storage/postgres/session.go` | SessionStore + 错误转换 |
| `internal/storage/postgres/task.go` | TaskStore |
| `internal/storage/postgres/runtask.go` | RunTaskStore |
| `internal/storage/postgres/postgres_test.go` | 12 个集成测试（env 门控） |

**错误转换**是持久化层的核心职责：

```go
func convertErr(err error, alreadyExistsMsg string) error {
    if errors.Is(err, sql.ErrNoRows) { return storage.ErrNotFound }
    if isUniqueViolation(err) { ... return storage.ErrAlreadyExists }
    return err
}
```

上层只认 `storage.ErrNotFound` / `ErrAlreadyExists`，无需知道 SQLSTATE 23505。

**RunTask 的唯一冲突需要区分两种来源**：

- 同 ID 重复 → `ErrAlreadyExists`
- 同 Task 已有别的 RunTask → `ErrRunTaskExists`

两者都返回 23505，因此插入冲突后用「该 ID 是否已存在」再判定一次。

### 集成测试的运行方式

```bash
# 创建测试库
psql -U postgres -c "CREATE DATABASE dra_test;"

# 运行（未设置 DSN 时自动 skip）
DRA_TEST_PG_DSN="postgres://postgres:pass@127.0.0.1:5432/dra_test?sslmode=disable" \
  go test ./internal/storage/postgres/ -v
```

**注意**：测试用 `TRUNCATE ... RESTART IDENTITY CASCADE` 清库，
只能指向专用测试库，不要指向开发库。

---

## 7. 待确认的决策

| # | 问题 | 建议 |
|---|---|---|
| D1 | `History` 是否需要分页接口？ | 当前 `Get` 全量读；超过 1000 条再考虑 |
| D2 | `ResearchState.Get` 是否拆成 `GetMeta` + `GetFull`？ | 先不拆，等有性能问题再说 |
| D3 | 枚举字段是否加 `CHECK` 约束？ | **建议加**，开销小、防脏数据 |
| D4 | 乐观锁冲突如何处理？ | 记日志，不重试 |
| D5 | Checkpoint 是否入库？ | **建议不入**，用 Redis 或文件 |
| D6 | 是否需要数据迁移脚本？ | 当前无生产数据，不需要 |

---

## 8. 与内存实现的语义对齐清单

实现 PG 版时，以下行为**必须与内存实现一致**（已有测试覆盖）：

- [ ] `Create` 主键冲突 → 返回 `storage.ErrAlreadyExists`（见下方说明）
- [ ] `Update` 目标不存在 → 返回 `ErrNotFound`
- [ ] `Delete` 目标不存在 → 返回 `ErrNotFound`（除级联删除外幂等）
- [ ] `ListByRunTask` 返回**按插入顺序**（内存实现用 `seq` 切片；
      PG 版用 `created_at ASC, id ASC` 保证同毫秒时也稳定）
- [ ] `Get` 返回的对象**修改后不影响存储**（内存实现靠深拷贝，PG 天然满足）
- [ ] `RevokeResult` 返回被撤销的条数
- [ ] `Commit` 在门控关闭时返回 `researchstate.ErrNotAccepting`
- [ ] `SetConclusion` 会写 `commit_log`
- [ ] 空 `time.Time{}` 读写往返后仍是零值（PG 存 NULL）

### ⚠️ 已知不一致（需在阶段 0 修复）

内存实现的 `Create` 冲突**没有使用哨兵错误**：

```go
// storage/memory/memory.go
return fmt.Errorf("storage: assignment %q already exists", a.ID)   // ← 无法 errors.Is
return fmt.Errorf("storage: work %q already exists", w.ID)
```

而 `storage` 包里其实定义了 `ErrRunTaskExists` 却没有通用的 `ErrAlreadyExists`。

**阶段 0 必须先统一**：新增 `storage.ErrAlreadyExists`，
三个 Store 的 `Create` 冲突都用它包装。否则 PG 版无法复用同一套
`errors.Is` 判断，调用方只能比对字符串（违反错误手册的约定）。

---

## 9. 配置项（建议）

```
DRA_DB_ENABLED     = false          // 默认仍用内存，便于本地开发
DRA_DB_DSN         = "postgres://dra:dra@127.0.0.1:5432/dra?sslmode=disable"
DRA_DB_MAX_OPEN    = 20
DRA_DB_MAX_IDLE    = 10
DRA_DB_CONN_MAX_LIFETIME = 1800     // 秒
```

`pgx/stdlib` 会自动处理 `TIMESTAMPTZ` ↔ `time.Time`，
无需 MySQL 那样的 `parseTime=true`。

---

## 附录 A：从 MySQL 方案迁移的差异

若将来需要同时支持两者，以下是方言差异对照：

| 项 | MySQL | PostgreSQL |
|---|---|---|
| 布尔 | `TINYINT(1)` | `BOOLEAN` |
| 时间 | `DATETIME(3)` | `TIMESTAMPTZ` |
| 大文本 | `MEDIUMTEXT` | `TEXT` |
| 二进制 | `LONGBLOB` | `BYTEA` |
| 自增 | `AUTO_INCREMENT` | `GENERATED ALWAYS AS IDENTITY` |
| JSON | `JSON` | `JSONB`（可索引） |
| 字符串数组 | ❌ 需 JSON | `TEXT[]` |
| 部分索引 | ❌ 不支持 | ✅ `WHERE` 子句 |
| 插入返回 | 需再查 | `RETURNING` |
| 占位符 | `?` | `$1, $2, ...` |
| 定长字符 | `CHAR(n)` 补空格 | 同（**建议都改用 `TEXT`**） |

**结论**：若想减少未来迁移成本，**统一使用 `TEXT` 而非 `CHAR`/`VARCHAR`**，
仅用 `CHECK` 约束长度。这样 SQL 在两库间的差异会更小。
