package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/notify"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// helpers.go：deployctl 共享辅助（hooks / 审计 / 干预单 / 开库；从 main.go 拆出，纯机械搬迁）。

// hooks 组装留痕钩子（audit_logs + interventions + 告警）。
func hooks(cfg *Config, database *gorm.DB, notifier notify.Notifier) deploy.Hooks {
	return deploy.Hooks{
		Audit: func(ctx context.Context, actor, action, target string, detail any) {
			audit(ctx, database, actor, action, target, detail)
		},
		Intervene: func(ctx context.Context, target, kind, note string) error {
			return createIntervention(ctx, database, target, kind, note)
		},
		Alert: func(ctx context.Context, title, body, level string) {
			_ = notifier.SendAlert(ctx, notify.Alert{Title: title, Body: body, Level: level})
		},
	}
}

// audit 写一条审计日志（audit_logs 只增不删；DDL 与 approval.ensureTables 一致）。
// database 为 nil（DB 降级）时跳过。
func audit(ctx context.Context, database *gorm.DB, actor, action, target string, detail any) {
	if database == nil {
		return
	}
	var detailJSON string
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	if err := database.WithContext(ctx).Create(&db.AuditLog{
		Actor: actor, Action: action, Target: target, Detail: detailJSON,
	}).Error; err != nil {
		log.Printf("ERROR: write audit_log (%s %s %s): %v", actor, action, target, err)
	}
}

// createIntervention 写一条干预记录（interventions 表；DDL 与 approval.ensureTables 一致）。
// database 为 nil（DB 降级）时跳过（返回 nil，不阻断部署）。
func createIntervention(ctx context.Context, database *gorm.DB, target, kind, note string) error {
	if database == nil {
		return nil
	}
	return database.WithContext(ctx).Create(&db.Intervention{
		WorkflowID: target, Kind: kind, Status: "open", Note: note, CreatedAt: time.Now(),
	}).Error
}

func openDB(cfg *Config) (*gorm.DB, error) {
	gormDB, err := gorm.Open(mysql.Open(fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		cfg.MySQLUser, cfg.MySQLPassword, cfg.MySQLHost, cfg.MySQLPort, cfg.MySQLDatabase,
	)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	return gormDB, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "deployctl: "+format+"\n", args...)
	os.Exit(1)
}
