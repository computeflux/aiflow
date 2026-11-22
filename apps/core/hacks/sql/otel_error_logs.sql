-- P0 可观测性：error_logs 表增加 trace_id 列，打通错误与链路追踪
-- 幂等化（S1 起 bootstrap.sh 会重复执行本目录全部 SQL）
DROP PROCEDURE IF EXISTS p0_alter_error_logs;
DELIMITER $$
CREATE PROCEDURE p0_alter_error_logs()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'error_logs' AND COLUMN_NAME = 'trace_id') THEN
    ALTER TABLE error_logs ADD COLUMN trace_id VARCHAR(64) NOT NULL DEFAULT '' AFTER run_id,
      ADD KEY idx_trace (trace_id);
  END IF;
END$$
DELIMITER ;
CALL p0_alter_error_logs();
DROP PROCEDURE p0_alter_error_logs;
