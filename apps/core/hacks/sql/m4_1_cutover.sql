-- M4-1: 跨服务切流状态表 skill_cutover（工作项 wi-1795489658-2d59）
--
-- x-scan / like 的能力从 pycraper / pyreplay 收口到 social：同一个 skill 名下新旧两版
-- 实现契约相同、只有 addr 不同，所以切流的完整状态（当前档位 / 目标 addr / 判定阈值 /
-- 收尾时刻）单独记在 skill_cutover 一行里。分表的原因见 internal/db/models_skills.go 的
-- SkillCutover 注释：board 运维中心与审计要能一眼看清「切到第几档、达标没有、谁批的」，
-- 不能让人从多个 skill_versions.traffic_split 里反推意图。
--
-- 三列的语义（护栏判定直接读它们，写错一个数就会让放量放行）：
--   stage_at     当前档位窗口起点 → 相邻两档的最短观察间隔（CUTOVER_MIN_STAGE_SEC）；
--   completed_at 全量收尾时刻 → 旧服务下线的「稳定观察窗」起点（DECOMMISSION_STABLE_SEC）。
--     绝不能用 updated_at 当窗口起点：收尾之后任何一次改 note/actor 的写操作都会把它推后，
--     观察窗会被无限延长，旧服务永远停不掉。
--   traffic      当前档位（0/10/50/100），与 skill_versions.traffic_split 同步写。
--
-- 新库由 API 启动时 CutoverService.EnsureSchema()（GORM）建表；本脚本覆盖**只跑
-- bootstrap.sh 不跑 Go 服务**的环境，并给老库补齐后加的 completed_at / actor 列
-- （GORM AutoMigrate 只在跑 Go 服务的启动路径上补列）。
--
-- ⚠️ 本脚本的列名必须与 internal/db/models_skills.go 的 SkillCutover 逐列一致（版本号两列
-- 已显式钉成 column:target_version / column:legacy_version）。两条建表路径产出不同的表时，
-- EnsureSchema 的 HasTable 守卫会跳过建表（不补列），GORM 只能按模型列名生成 SQL →
-- MySQL 1054 Unknown column，切流 API 全挂，且只在运行时暴露。
-- 这条约束由单测钉住：migration.TestSkillCutoverDDLMatchesGORMColumns 逐列比对两边。
-- 幂等写法同 x_accounts_login_columns.sql：MySQL 的 ALTER TABLE 没有 ADD COLUMN
-- IF NOT EXISTS，判列存在只能走存储过程；DELIMITER 是客户端指令，hacks/bootstrap.sh
-- 用 mysql CLI 执行，原生支持。
CREATE TABLE IF NOT EXISTS skill_cutover (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  skill_name VARCHAR(128) NOT NULL UNIQUE,
  target_addr VARCHAR(512) NOT NULL,                 -- 新实现 addr（如 http://social:8010）
  target_version VARCHAR(32) NOT NULL DEFAULT '',    -- 切流目标版本号（Stage 时落定）
  legacy_version VARCHAR(32) NOT NULL DEFAULT '',    -- 被切流的旧版本号（回滚时恢复它）
  traffic INT NOT NULL DEFAULT 0,                    -- 当前档位 0 | 10 | 50 | 100
  status VARCHAR(16) NOT NULL DEFAULT 'pending',      -- pending | running | completed | rolled_back
  stage_at DATETIME(3) NULL,                         -- 当前档位窗口起点（时长护栏）
  completed_at DATETIME(3) NULL,                     -- 全量收尾时刻（下线稳定观察窗起点）
  parity_min DOUBLE NOT NULL DEFAULT 0,              -- 一致率阈值（0 = 回落 .env 缺省）
  success_min DOUBLE NOT NULL DEFAULT 0,             -- 成功率阈值（0 = 回落 .env 缺省）
  min_samples INT NOT NULL DEFAULT 0,                -- 每档最少样本（0 = 回落 .env 缺省）
  actor VARCHAR(64) NOT NULL DEFAULT '',             -- 最近一次操作者
  note TEXT NULL,                                    -- 人读说明/原因
  created_at DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP PROCEDURE IF EXISTS skill_cutover_migrate_columns;
DELIMITER $$
CREATE PROCEDURE skill_cutover_migrate_columns()
BEGIN
  -- 表由上面的 CREATE TABLE IF NOT EXISTS 保证存在；这里只补「老库缺列」的情况
  -- （早期版本的 skill_cutover 没有 completed_at / actor，GORM Save 会直接 1054）。
  -- 逐列独立判断 + 逐列一条 ALTER：任一补丁列已存在时只跳过它自己，绝不连带让别的列失败
  -- （见下面 note 的注释；形状由 migration.TestSkillCutoverLegacyColumnPatches 钉住）。
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_cutover' AND COLUMN_NAME = 'completed_at') THEN
    ALTER TABLE skill_cutover
      ADD COLUMN completed_at DATETIME(3) NULL AFTER stage_at;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_cutover' AND COLUMN_NAME = 'actor') THEN
    ALTER TABLE skill_cutover
      ADD COLUMN actor VARCHAR(64) NOT NULL DEFAULT '' AFTER min_samples;
  END IF;
  -- note 必须独立判断，绝不能挂在 actor 的存在性上：
  --   - 老库有 note 缺 actor → 合并成一条 ALTER 会同时 ADD note → 1060 Duplicate column name，
  --     整条 ALTER 失败 → CALL 失败 → bootstrap.sh 中断，恰好砸在脚本要救的那类老库上；
  --   - 老库有 actor 缺 note → note 永远补不上，后续 GORM Save 仍 1054。
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'skill_cutover' AND COLUMN_NAME = 'note') THEN
    ALTER TABLE skill_cutover
      ADD COLUMN note TEXT NULL AFTER actor;
  END IF;
END$$
DELIMITER ;
CALL skill_cutover_migrate_columns();
DROP PROCEDURE skill_cutover_migrate_columns;
