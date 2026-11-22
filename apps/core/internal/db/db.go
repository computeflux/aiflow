// Package db 是全系统 MySQL 访问层：GORM 连接、AutoMigrate、按域拆分的表模型（models*.go）。
package db

import (
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/wetee/ai-workflow/internal/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB 包装 *gorm.DB，承载 MySQL 连接与 AutoMigrate 入口。
type DB struct {
	*gorm.DB
}

// Open 按配置连接 MySQL（结构化 DSN + 连接/读写超时）并返回 DB。
func Open(cfg *config.Config) (*DB, error) {
	// 结构化构建 DSN：密码自动转义，并显式设置连接/读写超时。
	// 参数与之前的 DSN 保持一致：charset=utf8mb4、parseTime=true。
	mysqlCfg := mysqldriver.Config{
		User:         cfg.MySQLUser,
		Passwd:       cfg.MySQLPassword,
		Net:          "tcp",
		Addr:         fmt.Sprintf("%s:%s", cfg.MySQLHost, cfg.MySQLPort),
		DBName:       cfg.MySQLDatabase,
		Params:       map[string]string{"charset": "utf8mb4"},
		ParseTime:    true,
		Timeout:      5 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	gormDB, err := gorm.Open(mysql.Open(mysqlCfg.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	// 连接池配置：限制并发连接数并回收长期空闲/超龄连接，避免连接耗尽。
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	sqlDB.SetConnMaxIdleTime(1 * time.Minute)

	if err := gormDB.AutoMigrate(
		&Account{},
		&RawItem{},
		&PublishJob{},
		// AuditLog 不加入 AutoMigrate：S1 起表结构由 apps/core/hacks/sql/approvals.sql 管理
		// （月分区 + 复合主键 (id, created_at)，GORM 无法表达），见 models.go 注释。
		&FetchCache{},
		&IngestTask{},
		&EmailBatch{},
		&EmailRecipient{},
		&EmailTemplate{},
		&EmailSender{},
		&Board{},
		&BoardList{},
		&BoardCard{},
		&BoardComment{},
		&MindMap{},
		&SocialSchedule{},
		&SocialTweet{},
		&EmailSendLog{},
		&XAccount{},
		&FollowTarget{},
		&LikeJob{},
		&ErrorLog{},
		&WorkItem{}, // S9 统一工作项（docs/plan/S9-统一工作项流水线.md）
	); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return &DB{gormDB}, nil
}
