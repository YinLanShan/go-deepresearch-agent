-- DeepResearch Agent schema (PostgreSQL 14+)
--
-- 设计说明见 docs/DataModel.md。
-- 本文件可重复执行：所有对象使用 IF NOT EXISTS。

-- ─── Session ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT        PRIMARY KEY CHECK (length(id) = 32),
  title      TEXT        NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_created ON sessions (created_at);

-- ─── Task ───────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tasks (
  id          TEXT        PRIMARY KEY CHECK (length(id) = 32),
  session_id  TEXT        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
  user_id     TEXT        NOT NULL DEFAULT '',
  title       TEXT        NOT NULL DEFAULT '',
  description TEXT,
  enabled     BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks (session_id, created_at);

-- ─── RunTask ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS run_tasks (
  id            TEXT        PRIMARY KEY CHECK (length(id) = 32),
  task_id       TEXT        NOT NULL UNIQUE REFERENCES tasks (id) ON DELETE CASCADE,
  status        TEXT        NOT NULL,
  turn          INTEGER     NOT NULL DEFAULT 0,
  state_version BIGINT      NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_tasks_status ON run_tasks (status);

-- 只索引活跃运行：全局单例检查走这条，索引体积更小
CREATE INDEX IF NOT EXISTS idx_run_tasks_active ON run_tasks (status)
  WHERE status IN ('running', 'waiting');

-- ─── ResearchState ──────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS research_states (
  id                  TEXT        PRIMARY KEY CHECK (length(id) = 32),
  task_id             TEXT        NOT NULL UNIQUE REFERENCES tasks (id) ON DELETE CASCADE,
  session_id          TEXT        NOT NULL,
  ordinal             INTEGER     NOT NULL DEFAULT 1,

  conclusion          TEXT,
  interrupted_context TEXT,

  requirements        TEXT[]      NOT NULL DEFAULT '{}',
  related_nodes       JSONB       NOT NULL DEFAULT '[]',

  version             BIGINT      NOT NULL DEFAULT 0,
  created_at          TIMESTAMPTZ NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_states_session ON research_states (session_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_states_requirements ON research_states USING GIN (requirements);
CREATE INDEX IF NOT EXISTS idx_states_related ON research_states USING GIN (related_nodes);

-- ─── ResearchState 子表 ─────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS research_results (
  id            TEXT        PRIMARY KEY CHECK (length(id) = 32),
  state_id      TEXT        NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  assignment_id TEXT        NOT NULL DEFAULT '',
  turn          INTEGER     NOT NULL,
  status        TEXT        NOT NULL,
  content       TEXT        NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_results_state_assignment
  ON research_results (state_id, assignment_id);
CREATE INDEX IF NOT EXISTS idx_results_state_turn
  ON research_results (state_id, turn);

CREATE TABLE IF NOT EXISTS research_findings (
  id         TEXT        PRIMARY KEY CHECK (length(id) = 32),
  state_id   TEXT        NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  turn       INTEGER     NOT NULL,
  content    TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_findings_state_turn
  ON research_findings (state_id, turn);

CREATE TABLE IF NOT EXISTS research_commit_log (
  seq           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  state_id      TEXT        NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  op            TEXT        NOT NULL,
  turn          INTEGER     NOT NULL DEFAULT 0,
  assignment_id TEXT        NOT NULL DEFAULT '',
  result_id     TEXT        NOT NULL DEFAULT '',
  content       TEXT,
  reason        TEXT        NOT NULL DEFAULT '',
  version       BIGINT      NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_commitlog_state
  ON research_commit_log (state_id, seq);
CREATE INDEX IF NOT EXISTS idx_commitlog_op
  ON research_commit_log (op, created_at DESC);

CREATE TABLE IF NOT EXISTS research_messages (
  seq        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  state_id   TEXT        NOT NULL REFERENCES research_states (id) ON DELETE CASCADE,
  role       TEXT        NOT NULL,
  content    TEXT        NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_state
  ON research_messages (state_id, seq);

-- ─── Assignment / Work ──────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS assignments (
  id          TEXT        PRIMARY KEY CHECK (length(id) = 32),
  session_id  TEXT        NOT NULL,
  task_id     TEXT        NOT NULL,
  run_task_id TEXT        NOT NULL REFERENCES run_tasks (id) ON DELETE CASCADE,
  goal        TEXT        NOT NULL,
  status      TEXT        NOT NULL,
  attempt     INTEGER     NOT NULL DEFAULT 0,
  result      TEXT,
  err         TEXT,
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_assignments_runtask
  ON assignments (run_task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_assignments_status ON assignments (status);

-- 加速"是否还有未完成 Assignment"（HasPendingAssignments 热路径）
CREATE INDEX IF NOT EXISTS idx_assignments_pending ON assignments (run_task_id)
  WHERE status NOT IN ('completed', 'failed', 'cancelled');

CREATE TABLE IF NOT EXISTS works (
  id              TEXT        PRIMARY KEY CHECK (length(id) = 32),
  assignment_id   TEXT        NOT NULL REFERENCES assignments (id) ON DELETE CASCADE,
  session_id      TEXT        NOT NULL,
  task_id         TEXT        NOT NULL,
  run_task_id     TEXT        NOT NULL,
  goal            TEXT        NOT NULL,
  status          TEXT        NOT NULL,
  constraint_data JSONB,
  worker_id       TEXT        NOT NULL DEFAULT '',
  attempt         INTEGER     NOT NULL DEFAULT 1,
  stage           TEXT        NOT NULL DEFAULT '',
  detail          TEXT        NOT NULL DEFAULT '',
  result          TEXT,
  err             TEXT,
  started_at      TIMESTAMPTZ,
  finished_at     TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_works_runtask ON works (run_task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_works_assignment ON works (assignment_id);

-- ─── Checkpoint（可选，默认不启用）─────────────────────────────────────────
--
-- 说明（DataModel.md 2.11）：Eino checkpoint 是 gob 编码、可能数 MB、
-- 生命周期极短。建议放 Redis 或本地文件，而非主库。
-- 若确实要用 PG，请解除下面注释并配合定期清理。

-- CREATE TABLE IF NOT EXISTS checkpoints (
--   checkpoint_id TEXT        PRIMARY KEY,
--   data          BYTEA       NOT NULL,
--   updated_at    TIMESTAMPTZ NOT NULL
-- );
-- CREATE INDEX IF NOT EXISTS idx_checkpoints_updated ON checkpoints (updated_at);
