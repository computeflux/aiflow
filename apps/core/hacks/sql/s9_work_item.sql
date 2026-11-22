-- S9 统一工作项流水线（docs/plan/S9-统一工作项流水线.md §5.1）：
-- 需求 / Bug / 自进化提案统一成一种货币（work_items），进同一条带轻重缓急的队列，
-- 由 work-dispatch 调度器按 score 取最高分自动领取并驱动 S1 进化链，人只在审批处放行。
-- kind：requirement（需求）| bug（缺陷）；source：product | error_log | audit | manual。
-- 状态机：open→triaged→queued→in_progress→in_review→approved→shipped；
--         rejected / expired 为终止/旁路态（详见 internal/workitem）。
CREATE TABLE IF NOT EXISTS work_items (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  no VARCHAR(32) NOT NULL UNIQUE,              -- wi-<ts>-<seq>
  kind VARCHAR(16) NOT NULL,                  -- requirement | bug
  title VARCHAR(255) NOT NULL,
  description LONGTEXT,
  source VARCHAR(32) NOT NULL,                -- product | error_log | audit | manual
  source_ref VARCHAR(255) DEFAULT '',         -- card_id / error_log_id / audit_report_id / manual / 错误簇键（system|source|归一化消息，≤230 字符）
  target_type VARCHAR(32) NOT NULL,           -- skill | app | core
  target_name VARCHAR(128) DEFAULT '',
  score FLOAT DEFAULT 0,
  priority VARCHAR(8) DEFAULT 'P2',           -- P0 | P1 | P2 | P3
  severity TINYINT DEFAULT 0,                 -- 0-4
  urgency  TINYINT DEFAULT 0,                 -- 0-4
  impact   TINYINT DEFAULT 0,                 -- 0-4
  frequency TINYINT DEFAULT 0,                -- 0-4
  status VARCHAR(32) NOT NULL DEFAULT 'open', -- open|triaged|queued|in_progress|in_review|approved|shipped|rejected|expired
  due_at DATETIME,
  checklist TEXT,                             -- JSON [{text,done}]
  assignee VARCHAR(128) DEFAULT '',
  locked TINYINT(1) DEFAULT 0,
  triage_degraded TINYINT(1) DEFAULT 0,       -- LLM 不可用降级标注（可观测，不假装智能）
  card_id BIGINT,                             -- 关联看板卡
  evolve_task_no VARCHAR(32) DEFAULT '',      -- 关联 S1 进化单
  rollback_count TINYINT DEFAULT 0,           -- S1 灰度回滚次数（重修上限，超限转 rejected）
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  triaged_at DATETIME,
  picked_at DATETIME,
  shipped_at DATETIME,
  updated_at DATETIME ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 注：error_logs.work_item_no / cards.work_item_no 两个关联列由 GORM AutoMigrate
-- 在 workflow-api 启动时自动补齐（db.Open 的 AutoMigrate 已含 WorkItem/BoardCard/ErrorLog），
-- 故本文件不写 ALTER（MySQL 8.0 不支持 ADD COLUMN IF NOT EXISTS，手写会重复建列报错）。
