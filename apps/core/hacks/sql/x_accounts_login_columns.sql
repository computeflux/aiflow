-- M1-3: x_accounts 补登录回报列（工作项 wi-1795586435-e694）
--
-- X 登录链（apps/pyreplay/adapters/x_login.py 的 AccountManagerReporter）每次登录都要
-- 把结果写回账号池：成功写 auth_token/ct0 + last_login_at 并清空 last_login_error；
-- 失败只写 last_login_at + last_login_error（绝不覆盖账号原有登录态，也绝不为
-- 「从没登录成功的账号」建 enabled=1 的空壳行 —— AccountManager.load_eligible 只按
-- enabled = 1 AND machine_id 过滤，不校验 token 是否为空，空壳行会被挑去点赞并必然失败）。
--
-- 这两列同时加进了 Go 侧 GORM 模型（apps/core/internal/db/models_content.go 的
-- XAccount），新库由 AutoMigrate 直接建出；**老库**里表已存在且列不存在，GORM 的
-- AutoMigrate 会补，但如果某环境只跑本脚本（bootstrap.sh）不跑 Go 服务，就必须在这里补，
-- 否则 pyreplay 的 INSERT/UPDATE 会抛 (1054, "Unknown column 'last_login_at'")。
--
-- 幂等写法：MySQL 的 ALTER TABLE 没有 ADD COLUMN IF NOT EXISTS，判列存在只能走存储过程。
-- 过程体含分号，用客户端惯例的 DELIMITER 块声明边界（DELIMITER 是客户端指令、不是 SQL，
-- 不能进预处理语句；hacks/bootstrap.sh 用 mysql CLI 执行，原生支持 DELIMITER）。
-- DROP PROCEDURE IF EXISTS 前置 + 结尾清理，脚本可重复执行。
DROP PROCEDURE IF EXISTS x_accounts_migrate_login_columns;
DELIMITER $$
CREATE PROCEDURE x_accounts_migrate_login_columns()
BEGIN
  -- 表不存在时直接跳过：x_accounts 由 Go 侧 AutoMigrate 建（新库路径），
  -- 本脚本只负责给**已存在**的老库补列，不承担建表职责。
  IF EXISTS (SELECT 1 FROM information_schema.TABLES
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'x_accounts') THEN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'x_accounts' AND COLUMN_NAME = 'last_login_at') THEN
      ALTER TABLE x_accounts
        ADD COLUMN last_login_at DATETIME(3) NULL AFTER last_used_at,
        ADD COLUMN last_login_error TEXT NULL AFTER last_login_at;
    ELSEIF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'x_accounts' AND COLUMN_NAME = 'last_login_error') THEN
      ALTER TABLE x_accounts
        ADD COLUMN last_login_error TEXT NULL AFTER last_login_at;
    END IF;
  END IF;
END$$
DELIMITER ;
CALL x_accounts_migrate_login_columns();
DROP PROCEDURE x_accounts_migrate_login_columns;
