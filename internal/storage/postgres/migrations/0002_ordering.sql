-- 0002: 为子表补充插入顺序列
--
-- 背景：Commit 在一个事务内批量插入多条 research_results / research_findings，
-- 它们的 created_at 完全相同。仅靠 created_at + id 排序无法稳定还原插入顺序
-- （id 是随机 hex，排序结果不确定），而内存实现按切片追加顺序返回。
--
-- 因此增加 seq 列（GENERATED ALWAYS AS IDENTITY）作为权威插入顺序，
-- 读取时按 seq 升序，与内存实现在语义上完全对齐。

ALTER TABLE research_results
  ADD COLUMN IF NOT EXISTS seq BIGINT GENERATED ALWAYS AS IDENTITY;

ALTER TABLE research_findings
  ADD COLUMN IF NOT EXISTS seq BIGINT GENERATED ALWAYS AS IDENTITY;

-- 覆盖原先以 created_at 为主的排序索引。
CREATE INDEX IF NOT EXISTS idx_results_state_seq
  ON research_results (state_id, seq);
CREATE INDEX IF NOT EXISTS idx_findings_state_seq
  ON research_findings (state_id, seq);
