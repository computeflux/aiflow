-- 统一 utf8mb4 collation（2026-10-30）
--
-- 背景：库默认 collation 是 utf8mb4_unicode_ci，而 hacks/sql/*.sql 里手写的 DDL 用
-- `DEFAULT CHARSET=utf8mb4`（不带 COLLATE）→ MySQL 解析成**字符集默认** utf8mb4_0900_ai_ci。
-- 于是同一库里出现两套：DDL 建的表（work_items/projects/evolve_tasks/approvals…）是 0900，
-- GORM AutoMigrate 建的表（boards/board_cards/error_logs…）继承库默认 unicode_ci。
--
-- 后果：两侧的关联列无法直接 JOIN / 比较 ——
--   board_cards.work_item_no ↔ work_items.no
--   board_cards.source_ref / target_name ↔ work_items.source_ref / projects.name
--   error_logs.work_item_no ↔ work_items.no
-- 会报 `ERROR 1267 (HY000): Illegal mix of collations`。
--
-- 本脚本做两件事（均幂等）：
--   1. 把已存在的关联表转成 utf8mb4_0900_ai_ci（表不存在则跳过：全新库由 AutoMigrate 后续建，
--      建出来已经是 0900，无需处理）；
--   2. 库/服务器级默认 collation 由 deploy/compose/infra/mysql.yml 的
--      `--collation-server=utf8mb4_0900_ai_ci` 与 bootstrap.sh 的 `ALTER DATABASE` 负责
--      （ALTER DATABASE 不支持 prepared statement，只能带字面库名，放 shell 里用 $MYSQL_DATABASE）。
--
-- 注意：只转「跨表关联」的这几张。raw_items（433MB，爬取原始数据）等自成一体的表不动，
-- 转换大表要整表重建，收益为零。

DROP PROCEDURE IF EXISTS align_board_collation;
DELIMITER $$
CREATE PROCEDURE align_board_collation()
BEGIN
  DECLARE done INT DEFAULT 0;
  DECLARE tname VARCHAR(64);
  -- 只挑 collation 还不一致的表；不存在的表天然不在结果集里
  DECLARE cur CURSOR FOR
    SELECT TABLE_NAME FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME IN ('boards', 'board_lists', 'board_cards', 'board_comments', 'error_logs', 'audit_logs')
      AND TABLE_COLLATION IS NOT NULL
      AND TABLE_COLLATION <> 'utf8mb4_0900_ai_ci';
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;

  OPEN cur;
  read_loop: LOOP
    FETCH cur INTO tname;
    IF done THEN
      LEAVE read_loop;
    END IF;
    SET @s := CONCAT('ALTER TABLE `', tname,
                     '` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci');
    PREPARE ps FROM @s;
    EXECUTE ps;
    DEALLOCATE PREPARE ps;
  END LOOP;
  CLOSE cur;
END$$
DELIMITER ;

CALL align_board_collation();
DROP PROCEDURE align_board_collation;
