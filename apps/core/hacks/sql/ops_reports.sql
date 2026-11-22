-- S6 运维情报（docs/plan/S6-运维情报.md §3.1/§3.2）：
-- cmd/ops-report 每日 08:00 出日报、每周一 08:00 出升级建议；
-- content 为 Markdown，metrics 为聚合原始数据（JSON，供图表/对账/一键转任务卡）。
-- 唯一约束 (report_date, type)：同一日期同类型幂等 upsert，重启不产生重复行。
CREATE TABLE IF NOT EXISTS ops_reports (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  report_date DATE NOT NULL,
  type VARCHAR(32) NOT NULL,            -- daily | weekly | upgrade_suggestion
  content LONGTEXT NOT NULL,            -- Markdown
  metrics JSON NOT NULL,                -- 聚合原始数据（供图表/校验）
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_date_type (report_date, type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
