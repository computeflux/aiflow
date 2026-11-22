package approval

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// audit_logs 月分区管理（docs/plan/S1-进化闭环.md §3.5）。
//
// 约定：分区名 pYYYYMM 表示该月起始（VALUES LESS THAN 下月首日），
// p_max 为 MAXVALUE 兜底（覆盖未来，无需逐月新增分区）。
// 清理只 DROP 过期分区（保留 AUDIT_LOG_RETENTION_DAYS 天，默认 180），
// 不提供任何删除 API；MySQL 应用账号对 audit_logs 无 DELETE/DROP 权限。

var partitionNameRe = regexp.MustCompile(`^p(\d{6})$`)

type auditPartition struct {
	Name string `gorm:"column:PARTITION_NAME"`
	// PARTITION_DESCRIPTION：RANGE 分区为边界值（TO_DAYS 结果）
	Description string `gorm:"column:PARTITION_DESCRIPTION"`
}

// listAuditPartitions 返回 audit_logs 的分区（非 MAXVALUE 的才返回）。
func (s *Store) listAuditPartitions() ([]auditPartition, error) {
	var parts []auditPartition
	err := s.db.Raw(
		"SELECT PARTITION_NAME, PARTITION_DESCRIPTION FROM information_schema.PARTITIONS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'audit_logs' AND PARTITION_NAME IS NOT NULL",
	).Scan(&parts).Error
	if err != nil {
		return nil, fmt.Errorf("list audit partitions: %w", err)
	}
	return parts, nil
}

// isPartitioned 判断 audit_logs 是否已分区。
func (s *Store) isPartitioned() (bool, error) {
	parts, err := s.listAuditPartitions()
	if err != nil {
		return false, err
	}
	return len(parts) > 0, nil
}

// initAuditLogPartitions 首次分区初始化（幂等）：
// 分区范围 = 最早数据月 ~ 当前月 + p_max。已分区则直接返回。
func (s *Store) initAuditLogPartitions() error {
	partitioned, err := s.isPartitioned()
	if err != nil {
		return err
	}
	if partitioned {
		return nil
	}

	var min time.Time
	err = s.db.Raw("SELECT IFNULL(MIN(created_at), NOW()) FROM audit_logs").Scan(&min).Error
	if err != nil {
		return fmt.Errorf("query audit_logs min: %w", err)
	}

	start := time.Date(min.Year(), min.Month(), 1, 0, 0, 0, 0, time.Local)
	end := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Local)

	var ddl string
	for cur := start; !cur.After(end); cur = cur.AddDate(0, 1, 0) {
		if ddl != "" {
			ddl += ", "
		}
		next := cur.AddDate(0, 1, 0)
		ddl += fmt.Sprintf("PARTITION p%s VALUES LESS THAN (TO_DAYS('%s'))",
			cur.Format("200102"), next.Format("2006-01-02"))
	}
	ddl += ", PARTITION p_max VALUES LESS THAN (MAXVALUE)"
	ddl = "ALTER TABLE audit_logs PARTITION BY RANGE (TO_DAYS(created_at)) (" + ddl + ")"

	if err := s.db.Exec(ddl).Error; err != nil {
		return fmt.Errorf("partition audit_logs: %w", err)
	}
	return nil
}

// CleanupPartitions 删除过期月分区（起始日早于 now-retention 的分区）。
// 返回删除的分区名。只动分区，不做任何行级删除。
func (s *Store) CleanupPartitions(ctx context.Context) ([]string, error) {
	partitioned, err := s.isPartitioned()
	if err != nil {
		return nil, err
	}
	if !partitioned {
		return nil, s.initAuditLogPartitions()
	}

	parts, err := s.listAuditPartitions()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.AuditLogRetentionDays)

	var toDrop []string
	for _, p := range parts {
		m := partitionNameRe.FindStringSubmatch(p.Name)
		if m == nil { // p_max 等兜底分区不动
			continue
		}
		start, err := time.ParseInLocation("200601", m[1], time.Local)
		if err != nil {
			continue
		}
		if start.Before(cutoff) {
			toDrop = append(toDrop, p.Name)
		}
	}
	if len(toDrop) == 0 {
		return nil, nil
	}

	ddl := "ALTER TABLE audit_logs DROP PARTITION "
	for i, name := range toDrop {
		if i > 0 {
			ddl += ", "
		}
		ddl += name
	}
	if err := s.db.WithContext(ctx).Exec(ddl).Error; err != nil {
		return toDrop, fmt.Errorf("drop audit partitions %v: %w", toDrop, err)
	}
	return toDrop, nil
}
