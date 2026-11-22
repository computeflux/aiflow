// Package store 提供 GORM 数据库连接与 AutoMigrate。
package store

import (
	"fmt"
	"log"
	"time"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 连接 MySQL 并 AutoMigrate 全部 auth_ 表（+ 共用的 error_logs）。
func Open(cfg *config.Config) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(cfg.DBDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.Role{},
		&model.Permission{},
		&model.RolePermission{},
		&model.UserRole{},
		&model.UserPermission{},
		&model.OtpCode{},
		&model.RefreshToken{},
		&model.TokenBlacklist{},
		&model.AuditLog{},
		&model.ErrorLog{},
	); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}
	// 连接池上限 + 连接生命周期：防止连接泄漏/耗尽导致登录请求排队阻塞。
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)
	if pingErr := sqlDB.Ping(); pingErr != nil {
		log.Printf("[auth-store] db ping note: %v", pingErr)
	}
	return db, nil
}
