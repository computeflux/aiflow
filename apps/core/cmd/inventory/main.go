// Package main is the inventory binary.
//
// inventory 是 S5 软件资产画像工具（docs/plan/S5-软件接管与部署编排.md §3.1）：
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
	"github.com/wetee/ai-workflow/internal/deploy"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`
	RepoRoot      string `env:"REPO_ROOT" envDefault:"/work"`
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	database, err := openDB(cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	store := deploy.NewStore(database)
	if err := store.EnsureSchema(); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}

	ctx := context.Background()
	scanner := deploy.NewScanner(cfg.RepoRoot, database)
	assets, err := scanner.Scan(ctx)
	if err != nil {
		log.Fatalf("scan repo: %v", err)
	}
	if err := store.UpsertAll(ctx, assets); err != nil {
		log.Fatalf("upsert assets: %v", err)
	}

	managed, external := 0, 0
	fmt.Printf("=== software_assets: %d 资产（%s）===\n", len(assets), cfg.RepoRoot)
	for _, a := range assets {
		mark := "✖"
		if a.AutoManaged == 1 {
			mark, managed = "✔", managed+1
		} else {
			external++
		}
		fmt.Printf("  %s %-20s %-14s %-8s owner=%s\n", mark, a.Name, a.Kind, a.StartMethod, a.Owner)
	}
	fmt.Printf("纳管 %d / 不纳管 %d\n", managed, external)
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
	return gormDB, nil
}
