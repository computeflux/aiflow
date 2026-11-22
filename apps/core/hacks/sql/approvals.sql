-- S1 进化闭环数据库表（docs/plan/S1-进化闭环.md §3.1 / §3.5）
--
-- 与 plan 的差异（有意为之，均已注释说明）：
-- 1. approvals.kind / status 用 VARCHAR 而非 ENUM：与 skills.sql 同一约定
--    （避免 GORM 冲突与后续加状态值需要 ALTER）。
-- 2. approvals 增加 expires_at（S1 §3.2 的 24h 过期扫描必需）+ idx_approvals_status。
-- 3. audit_logs 为**扩展既有表**：老表（publish_jobs 审计）已有 job_id / note 列，
--    S1 需要 target / detail 列。本文件先 CREATE IF NOT EXISTS（全新库一次建齐
--    合并结构），再对老表幂等补列。月分区同样幂等初始化。
-- 4. 新增 agents_registry 表：plan 只有 agent_bindings（role→agent 绑定），
--    但 board「Agents」页需要"已注册 Agent"清单，注册 manifest 需要落表。
-- 5. skill_versions 增加 gray_stage / gray_stage_started_at / approval_id 三列：
--    灰度状态机（S1 §3.4）需要记录当前档位与窗口起点、审批单关联，
--    全部持久化在 skill_versions（plan 要求）。
-- 6. interventions 表按 transform-plan §9.3 的 DDL 提前建（S3 才建）：
--    S1 的越界打回与灰度回滚需要写干预记录。
--
-- 本文件由 hacks/bootstrap.sh 顺序执行，也可手工：
--   cd deploy && docker compose exec -T mysql mysql -uworkflow -pworkflow123 workflow < ../../hacks/sql/approvals.sql

-- 进化审批单（人类卡点）
CREATE TABLE IF NOT EXISTS approvals (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  approval_no VARCHAR(32) NOT NULL UNIQUE,
  kind VARCHAR(32) NOT NULL,                        -- skill_release | rollback | takeover
  subject VARCHAR(255) NOT NULL,
  payload JSON NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',    -- pending | approved | rejected | expired
  proposer VARCHAR(64) NOT NULL,
  approver VARCHAR(64),
  reason TEXT,
  expires_at DATETIME,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  decided_at DATETIME,
  KEY idx_approvals_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 审计日志（月分区，只增不删；应用账号仅 SELECT + INSERT）
CREATE TABLE IF NOT EXISTS audit_logs (
  id BIGINT AUTO_INCREMENT,
  job_id BIGINT NULL,                              -- 老列：publish job 审计
  actor VARCHAR(64) NOT NULL,
  action VARCHAR(64) NOT NULL,
  target VARCHAR(255) NOT NULL DEFAULT '',         -- S1 新增
  note TEXT,                                       -- 老列
  detail JSON NULL,                                -- S1 新增
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id, created_at)                     -- 月分区要求：PK 必须覆盖分区列
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 老库幂等补列 + PK 迁移（全新库上面 CREATE 已建齐，此处无操作）
DROP PROCEDURE IF EXISTS s1_alter_audit_logs;
DELIMITER $$
CREATE PROCEDURE s1_alter_audit_logs()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs' AND COLUMN_NAME = 'target') THEN
    ALTER TABLE audit_logs ADD COLUMN target VARCHAR(255) NOT NULL DEFAULT '' AFTER action;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs' AND COLUMN_NAME = 'detail') THEN
    ALTER TABLE audit_logs ADD COLUMN detail JSON NULL AFTER note;
  END IF;
  -- 老表 PK 为 (id) 单列：月分区要求 PK 覆盖分区列 created_at，迁移为 (id, created_at)
  IF (SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs'
          AND CONSTRAINT_NAME = 'PRIMARY') = 1
     AND (SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
           WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs'
             AND CONSTRAINT_NAME = 'PRIMARY' LIMIT 1) = 'id' THEN
    ALTER TABLE audit_logs DROP PRIMARY KEY, ADD PRIMARY KEY (id, created_at);
  END IF;
END$$
DELIMITER ;
CALL s1_alter_audit_logs();
DROP PROCEDURE s1_alter_audit_logs;

-- 月分区初始化（幂等：已分区则跳过；分区 = 最早数据月 ~ 当前月 + p_max 兜底）
DROP PROCEDURE IF EXISTS s1_init_audit_log_partitions;
DELIMITER $$
CREATE PROCEDURE s1_init_audit_log_partitions()
BEGIN
  DECLARE v_partitioned INT DEFAULT 0;
  DECLARE v_start DATE;
  DECLARE v_end DATE;
  DECLARE v_cur DATE;
  DECLARE v_ddl TEXT;

  SELECT COUNT(*) INTO v_partitioned
    FROM information_schema.PARTITIONS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs' AND PARTITION_NAME IS NOT NULL;

  IF v_partitioned = 0 THEN
    SELECT IFNULL(MIN(DATE(created_at)), CURDATE()) INTO v_start FROM audit_logs;
    SET v_start = DATE_FORMAT(v_start, '%Y-%m-01');
    SET v_end = DATE_FORMAT(CURDATE(), '%Y-%m-01');
    SET v_ddl = 'ALTER TABLE audit_logs PARTITION BY RANGE (TO_DAYS(created_at)) (';
    SET v_cur = v_start;
    WHILE v_cur <= v_end DO
      SET v_ddl = CONCAT(v_ddl, IF(v_cur > v_start, ', ', ''),
        'PARTITION p', DATE_FORMAT(v_cur, '%Y%m'), ' VALUES LESS THAN (TO_DAYS(',
        QUOTE(DATE_FORMAT(DATE_ADD(v_cur, INTERVAL 1 MONTH), '%Y-%m-%d')), '))');
      SET v_cur = DATE_ADD(v_cur, INTERVAL 1 MONTH);
    END WHILE;
    SET v_ddl = CONCAT(v_ddl, ', PARTITION p_max VALUES LESS THAN (MAXVALUE))');
    -- MySQL 9.x 不再支持存储程序内 PREPARE ... FROM 局部变量，改用会话用户变量
    SET @s1_ddl = v_ddl;
    PREPARE s1_stmt FROM @s1_ddl;
    EXECUTE s1_stmt;
    DEALLOCATE PREPARE s1_stmt;
  END IF;
END$$
DELIMITER ;
CALL s1_init_audit_log_partitions();
DROP PROCEDURE s1_init_audit_log_partitions;

-- Agent 角色绑定（transform-plan §4.4）
CREATE TABLE IF NOT EXISTS agent_bindings (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  role VARCHAR(32) NOT NULL UNIQUE,                -- coder | reviewer
  agent_name VARCHAR(128) NOT NULL,
  version VARCHAR(32) NOT NULL DEFAULT 'latest',
  updated_by VARCHAR(64),
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Agent 注册表（manifest 落表；board Agents 页 / agentctl list 数据源）
CREATE TABLE IF NOT EXISTS agents_registry (
  name VARCHAR(128) PRIMARY KEY,
  role VARCHAR(32) NOT NULL,                       -- coder | reviewer
  version VARCHAR(32) NOT NULL DEFAULT 'latest',
  entrypoint VARCHAR(512) NOT NULL,                -- 容器内 adapter 绝对路径
  container VARCHAR(64) NOT NULL,                  -- 运行 adapter 的容器（docker exec 目标）
  workdir_prefix VARCHAR(64) NOT NULL DEFAULT '/work',  -- 该容器内仓库根路径
  limits JSON NULL,
  permissions JSON NULL,
  config JSON NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 进化单（对应总纲 §6.2 的"<单号>"）
CREATE TABLE IF NOT EXISTS evolve_tasks (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  task_no VARCHAR(32) NOT NULL UNIQUE,          -- EV-20260928-001
  source VARCHAR(32) NOT NULL,                  -- patrol | manual | card
  evidence JSON,                                -- 审计证据 / 需求描述（含对自身的修改需求）
  target_type VARCHAR(32) NOT NULL,             -- skill | app
  target_name VARCHAR(128) NOT NULL,
  workdir VARCHAR(255),                         -- evolve-driver 容器内路径（/repo/...）
  status VARCHAR(32) NOT NULL DEFAULT 'open',   -- open/coding/reviewing/sandboxing/
                                                -- testing/approving/released/rolled_back/expired/cancelled
  diff LONGTEXT,
  review JSON,                                  -- reviewer 行级评论数组
  test_report JSON,                             -- {junit: <xml路径>, passed, failed, contract: ok}
  approval_id BIGINT,
  pr_url VARCHAR(255) DEFAULT '',                 -- 人工 PR（自动化）：审批通过后推分支 + 建 GitHub PR 的链接
  pr_error TEXT,                                  -- 建 PR/本地合并失败或跳过原因（非空 = 不再重试）；冲突文件列表很长
  merge_commit VARCHAR(64) DEFAULT '',            -- 本地合并（local 模式）：快进到 main 的提交 sha
  merge_state VARCHAR(32) DEFAULT '',             -- 合入阻塞态：'' = 正常；paused_base_dirty = main 工作区有未提交改动 → 暂停合入（每 5 分钟重试）
  session_id VARCHAR(128) DEFAULT '',             -- pi-web 会话（多轮续接）
  skip_review TINYINT(1) NOT NULL DEFAULT 0,      -- 1 = 跳过 AI reviewer（看板「跳过审核」），直接进 Review 列由人工看 diff
  fail_reason TEXT,                               -- 失败原因（status=failed）；failed 不打回 Backlog，卡留在 In Progress
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_evolve_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 熔断状态持久化（api 重启不丢状态；S3 熔断器启用）
CREATE TABLE IF NOT EXISTS skill_circuit (
  skill_name VARCHAR(128) PRIMARY KEY,
  state VARCHAR(16) NOT NULL DEFAULT 'close',   -- close | open | half
  consecutive_failures INT NOT NULL DEFAULT 0,
  opened_at DATETIME NULL,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 干预记录（transform-plan §9.3；S1 提前建：越界打回 + 灰度回滚写入）
CREATE TABLE IF NOT EXISTS interventions (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  workflow_id VARCHAR(64) NOT NULL,
  run_id VARCHAR(64),
  kind VARCHAR(32) NOT NULL,                     -- circuit_open | loop_detect | manual
  status VARCHAR(16) NOT NULL DEFAULT 'open',    -- open | resolved
  note TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  resolved_at DATETIME
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- skill_versions 灰度列（幂等补列）
DROP PROCEDURE IF EXISTS s1_alter_skill_versions;
DELIMITER $$
CREATE PROCEDURE s1_alter_skill_versions()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_versions' AND COLUMN_NAME = 'gray_stage') THEN
    ALTER TABLE skill_versions ADD COLUMN gray_stage TINYINT NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_versions' AND COLUMN_NAME = 'gray_stage_started_at') THEN
    ALTER TABLE skill_versions ADD COLUMN gray_stage_started_at DATETIME NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_versions' AND COLUMN_NAME = 'approval_id') THEN
    ALTER TABLE skill_versions ADD COLUMN approval_id BIGINT NULL;
  END IF;
END$$
DELIMITER ;
CALL s1_alter_skill_versions();
DROP PROCEDURE s1_alter_skill_versions;

-- audit_logs 权限收口（表级 GRANT 覆盖库级 ALL）在 grants.sql，
-- 由 bootstrap.sh 以 root 账号执行（应用账号无 GRANT 权限）。
