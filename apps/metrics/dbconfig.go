package main

// 配置数据库: 所有数据(监控主机/服务/可部署应用/续签/邮件告警)统一由
// MySQL monitor_config 表维护(单行 JSON, 面板可读写, 30s 热加载, 无回退)。
//
//   - 连接: MONITOR_DB_DSN 环境变量(完整 DSN) > 内置默认(宿主机 workflow 库);
//     连接信息本身不进配置, 不存在"数据库配置存数据库"的自指问题
//   - 播种: 首次启动表为空时, 用内置默认配置写入(请在面板设置页完善)
//   - 无回退: 数据库不可达时 HTTP 模式直接启动失败(容器按 restart 策略重试),
//     避免配置不一致的静默降级; CLI 模式仅降级为"无邮件告警"
//   - API:  GET/PUT /api/config(需 MONITOR_DEPLOY_TOKEN, 见 deploy.go)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const (
	monitorConfigTable = "monitor_config"
	configRefreshEvery = 30 * time.Second
	configDBTimeout    = 10 * time.Second
	configDDLTimeout   = 30 * time.Second // DDL(建表)可能在数据库恢复中排队等待元数据锁, 放宽
	openAttempts       = 5                // 首次启动重试次数(容忍数据库稍后启动/恢复)
	openRetryWait      = 5 * time.Second
)

// defaultDBDSN 是内置配置库连接(workflow 库), 可被 MONITOR_DB_DSN 覆盖。
const defaultDBDSN = "workflow:workflow123@tcp(dev-workflow.computeflux.ai:3306)/workflow"

// dbDSN 返回配置库 DSN(确保 parseTime=true, 否则 updated_at 无法扫入 time.Time)。
func dbDSN() string {
	dsn := os.Getenv("MONITOR_DB_DSN")
	if dsn == "" {
		dsn = defaultDBDSN
	}
	if !strings.Contains(dsn, "parseTime=") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "parseTime=true"
	}
	return dsn
}

// configStore 提供数据库配置的读写访问; get 返回的 *config 是不可变快照,
// 热加载/保存时整体原子替换, 调用方可安全持有旧指针。
type configStore interface {
	get() *config
	put(c *config) error
	updatedAt() time.Time
	startRefresh()
}

type dbConfigStore struct {
	mu       sync.RWMutex
	cfg      *config
	raw      string    // JSON 快照, 用于热加载时检测变更
	storedAt time.Time // 数据库行 updated_at

	db *sql.DB
}

// openConfigStore 连接配置库并加载配置行(空表则播种内置默认), 带有限重试:
// 容忍数据库稍后启动/恢复(容器编排常见); 重试耗尽仍失败则返回错误,
// 由调用方退出, 容器 restart 策略继续重试(保持无回退语义)。
func openConfigStore() (configStore, error) {
	var lastErr error
	for attempt := 1; attempt <= openAttempts; attempt++ {
		s, err := openConfigStoreOnce()
		if err == nil {
			return s, nil
		}
		lastErr = err
		if attempt < openAttempts {
			fmt.Printf("[metrics] 配置数据库暂不可用 (%d/%d): %v, %s 后重试\n", attempt, openAttempts, err, openRetryWait)
			time.Sleep(openRetryWait)
		}
	}
	return nil, lastErr
}

// openConfigStoreOnce 单次尝试: 连接 → 建表(如不存在) → 加载配置行。
// 各步骤使用独立超时上下文: 慢步骤(如数据库恢复中 DDL 排队)不会挤占后续步骤的预算。
func openConfigStoreOnce() (configStore, error) {
	db, err := sql.Open("mysql", dbDSN())
	if err != nil {
		return nil, fmt.Errorf("config db: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), configDBTimeout)
	err = db.PingContext(ctx)
	cancel()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("配置数据库不可达 (%s): %w", dsnLabel(dbDSN()), err)
	}

	const createSQL = `CREATE TABLE IF NOT EXISTS ` + monitorConfigTable + ` (
		id TINYINT UNSIGNED NOT NULL DEFAULT 1,
		data JSON NOT NULL,
		updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
		PRIMARY KEY (id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	ctx, cancel = context.WithTimeout(context.Background(), configDDLTimeout)
	_, err = db.ExecContext(ctx, createSQL)
	cancel()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("config db create table: %w", err)
	}

	s := &dbConfigStore{db: db}
	ctx, cancel = context.WithTimeout(context.Background(), configDBTimeout)
	err = s.load(ctx)
	cancel()
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsnLabel 返回可打印的连接标识(不含密码), 用于日志/错误信息。
func dsnLabel(dsn string) string {
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		user := dsn[:at]
		if colon := strings.Index(user, ":"); colon >= 0 {
			user = user[:colon]
		}
		return user + "@" + dsn[at+1:]
	}
	return dsn
}

// defaultConfig 是首次播种用的内置默认配置(请随后在面板设置页完善)。
func defaultConfig() *config {
	return &config{
		Hosts:    splitHosts(defaultHosts),
		Port:     443,
		WarnDays: 30,
		Interval: "1h",
		Renewal:  renewalConfig{Days: 10, DNS: "alidns"},
	}
}

// load 读取配置行; 表为空时先用内置默认播种。
func (s *dbConfigStore) load(ctx context.Context) error {
	var (
		raw string
		at  time.Time
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT data, updated_at FROM `+monitorConfigTable+` WHERE id = 1`).Scan(&raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		cfg := defaultConfig()
		b, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO `+monitorConfigTable+` (id, data) VALUES (1, ?)`, b); err != nil {
			return fmt.Errorf("config db seed: %w", err)
		}
		s.apply(string(b), time.Now())
		fmt.Printf("[metrics] %s 表为空, 已播种内置默认配置, 请在面板设置页完善\n", monitorConfigTable)
		return nil
	}
	if err != nil {
		return fmt.Errorf("config db read: %w", err)
	}
	s.apply(raw, at)
	return nil
}

// apply 解析 JSON 快照并原子替换内存配置; 解析失败保留旧配置并告警。
func (s *dbConfigStore) apply(raw string, at time.Time) {
	var c config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		fmt.Printf("[metrics] 配置行 JSON 无效, 保留当前内存配置: %v\n", err)
		return
	}
	s.mu.Lock()
	s.cfg = &c
	s.raw = raw
	s.storedAt = at
	s.mu.Unlock()
}

// get 返回当前内存配置快照。
func (s *dbConfigStore) get() *config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *dbConfigStore) updatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storedAt
}

// put 校验并写入配置, 再回读数据库(取数据库侧 updated_at)刷新内存快照。
func (s *dbConfigStore) put(c *config) error {
	if len(c.Hosts) == 0 {
		return errors.New("hosts 不能为空")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`REPLACE INTO `+monitorConfigTable+` (id, data) VALUES (1, ?)`, b); err != nil {
		return fmt.Errorf("config db write: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), configDBTimeout)
	defer cancel()
	return s.refresh(ctx)
}

// refresh 读取数据库行, 有变更则热加载; 失败返回错误由调用方决定日志策略。
func (s *dbConfigStore) refresh(ctx context.Context) error {
	var (
		raw string
		at  time.Time
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT data, updated_at FROM `+monitorConfigTable+` WHERE id = 1`).Scan(&raw, &at)
	if err != nil {
		return fmt.Errorf("config db read: %w", err)
	}
	s.mu.RLock()
	changed := raw != s.raw
	s.mu.RUnlock()
	if changed {
		s.apply(raw, at)
	}
	return nil
}

// startRefresh 后台每 30s 轮询数据库, 面板保存的配置无需重启即生效。
// 轮询失败(如数据库瞬断)仅记录日志, 保留当前内存配置, 服务不中断。
func (s *dbConfigStore) startRefresh() {
	go func() {
		ticker := time.NewTicker(configRefreshEvery)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), configDBTimeout)
			if err := s.refresh(ctx); err != nil {
				fmt.Printf("[metrics] 配置热加载失败(保留当前配置): %v\n", err)
			}
			cancel()
		}
	}()
}

// ---------------------------------------------------------------------------
// HTTP API: GET/PUT /api/config(面板"设置"页)
// ---------------------------------------------------------------------------

// respondAuthError 统一的 token 缺失/无效响应。
func respondAuthError(dep *deployer, w http.ResponseWriter) {
	if dep.token == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "配置读写未启用: 请设置环境变量 MONITOR_DEPLOY_TOKEN")
		return
	}
	writeJSONError(w, http.StatusUnauthorized, "部署 token 无效")
}

// handleConfigGet GET /api/config → {config, updated_at}(需 token)。
func handleConfigGet(store configStore, dep *deployer, w http.ResponseWriter, r *http.Request) {
	if !dep.authorize(r) {
		respondAuthError(dep, w)
		return
	}
	writeJSON(w, map[string]any{
		"config":     store.get(),
		"updated_at": store.updatedAt().UTC().Format(time.RFC3339),
	})
}

// handleConfigPut PUT /api/config(JSON body) → {ok, updated_at}(需 token, 校验 hosts 非空)。
func handleConfigPut(store configStore, dep *deployer, w http.ResponseWriter, r *http.Request) {
	if !dep.authorize(r) {
		respondAuthError(dep, w)
		return
	}
	var c config
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	if err := store.put(&c); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"ok":         true,
		"updated_at": store.updatedAt().UTC().Format(time.RFC3339),
	})
}
