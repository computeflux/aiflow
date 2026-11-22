-- P1 Skill 框架数据库表（docs/plan/transform-plan.md §6.3）
--
-- 与 plan 的差异（有意为之，均已注释说明）：
-- 1. skills 表增加了运行时必需列：entrypoint / addr / container / limits / permissions / updated_at。
--    plan §6.3 的 skills 表只有 name/owner/runtime/type/input_schema/output_schema，
--    但 §6.4 的 invoke 流程需要 addr（service 型 HTTP 调用）、entrypoint（cli 型执行）、
--    limits（限流/超时），skill.json（§6.1）也包含这些字段——表结构必须能承载它们，
--    否则 Gateway 无法从数据库恢复调用配置。
-- 2. status 用 VARCHAR(16) 而非 ENUM：避免 GORM AutoMigrate 与 ENUM 冲突，
--    也避免后续加状态值需要 ALTER。取值范围与 plan 一致：draft/testing/gray/live/retired。
-- 3. skill_runs.status 用 VARCHAR(16) 而非 ENUM，取值：ok/fail/timeout/rejected。
-- 4. skill_versions.config（P1 追加）：每个版本的完整 spec JSON 快照，
--    版本转 live 时用它更新 skills 行的运行配置，draft 版本不影响生产。
--
-- 本文件可手工执行，也可以直接依赖 API 启动时 skillgateway.EnsureSchema()
-- 自动建表（CREATE TABLE IF NOT EXISTS + 缺列 ALTER）。

CREATE TABLE IF NOT EXISTS skills (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  owner VARCHAR(64) NOT NULL,
  runtime VARCHAR(16) NOT NULL,
  type VARCHAR(16) NOT NULL,
  -- ↓ plan §6.3 未包含、§6.4 invoke 必需的扩展列
  entrypoint VARCHAR(512) NOT NULL DEFAULT '',
  addr VARCHAR(512) NOT NULL DEFAULT '',
  container VARCHAR(64) NOT NULL DEFAULT '',
  limits JSON NOT NULL,
  permissions JSON NULL,
  -- ↑ 扩展列结束
  input_schema JSON NOT NULL,
  output_schema JSON NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS skill_versions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  skill_id BIGINT NOT NULL,
  version VARCHAR(32) NOT NULL,
  image VARCHAR(255) NOT NULL,
  traffic_split INT DEFAULT 100,
  status VARCHAR(16) DEFAULT 'draft',
  rollback_to VARCHAR(32),
  -- ↓ P1 追加：完整 spec JSON 快照（转 live 时同步到 skills 行）
  config JSON NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_skill_version (skill_id, version),
  KEY idx_skill_versions_status (skill_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS skill_runs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  skill_name VARCHAR(128) NOT NULL,
  version VARCHAR(32) NOT NULL,
  trace_id VARCHAR(64) NOT NULL,
  workflow_id VARCHAR(64), run_id VARCHAR(64),
  input_hash CHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL,
  latency_ms INT, error TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  KEY idx_skill_time (skill_name, created_at),
  KEY idx_trace (trace_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
