// Package config 加载 auth 服务配置（环境变量 + .env）。
package config

import (
	"fmt"
	"log"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config 承载 auth 服务全部配置。
type Config struct {
	AuthHost string `env:"AUTH_HOST" envDefault:"0.0.0.0"`
	AuthPort string `env:"AUTH_PORT" envDefault:"8008"`

	// 数据库：AUTH_DB_DSN 优先；为空则用 MYSQL_*（根 .env 的 workflow 库）拼装。
	AuthDBDSN     string `env:"AUTH_DB_DSN"`
	MySQLHost     string `env:"MYSQL_HOST" envDefault:"127.0.0.1"`
	MySQLPort     string `env:"MYSQL_PORT" envDefault:"3306"`
	MySQLUser     string `env:"MYSQL_USER" envDefault:"root"`
	MySQLPassword string `env:"MYSQL_PASSWORD"`
	MySQLDatabase string `env:"MYSQL_DATABASE" envDefault:"workflow"`

	// JWT：RS256 私钥文件（不存在时首次启动自动生成，0600，不落库）。
	AuthJWTPrivateKeyPath string `env:"AUTH_JWT_PRIVATE_KEY_PATH" envDefault:"auth_jwt.key"`
	AuthJWTKid            string `env:"AUTH_JWT_KID"` // 缺省由公钥指纹推导
	AuthIssuer            string `env:"AUTH_ISSUER" envDefault:"https://auth.computeflux.ai"`
	AuthAudience          string `env:"AUTH_AUDIENCE" envDefault:"workflow"`
	AuthAccessTTLSec      int    `env:"AUTH_ACCESS_TTL_SEC" envDefault:"900"`
	AuthRefreshTTLSec     int    `env:"AUTH_REFRESH_TTL_SEC" envDefault:"2592000"`

	// 管理：X-Admin-Token（服务间管理操作）+ 首个管理员邮箱（首次登录自动授 admin）。
	AuthAdminToken  string `env:"AUTH_ADMIN_TOKEN"`
	AuthAdminEmail  string `env:"AUTH_ADMIN_EMAIL"`
	AuthAdminWallet string `env:"AUTH_ADMIN_WALLET"`

	// CORS 允许的跨域源（逗号分隔；留空则 *，不开 credentials——鉴权走 Bearer 头）。
	AuthCORSOrigins string `env:"AUTH_CORS_ORIGINS"`
	AuthLogFile     string `env:"AUTH_LOG_FILE" envDefault:"./logs/auth.log"`

	// SMTP（发 OTP 邮件；复用根 .env 的 SMTP_*）。
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     string `env:"SMTP_PORT" envDefault:"465"`
	SMTPUser     string `env:"SMTP_USER"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPFrom     string `env:"SMTP_FROM"`

	// 社交登录 OAuth 凭据（缺省的 provider 不可用，登录时返回明确错误）。
	AuthGoogleClientID     string `env:"AUTH_GOOGLE_CLIENT_ID"`
	AuthGoogleClientSecret string `env:"AUTH_GOOGLE_CLIENT_SECRET"`
	AuthGitHubClientID     string `env:"AUTH_GITHUB_CLIENT_ID"`
	AuthGitHubClientSecret string `env:"AUTH_GITHUB_CLIENT_SECRET"`
	AuthXClientID          string `env:"AUTH_X_CLIENT_ID"`
	AuthXClientSecret      string `env:"AUTH_X_CLIENT_SECRET"`
}

// DBDSN 返回 MySQL DSN（AUTH_DB_DSN 优先，否则由 MYSQL_* 拼装）。
func (c *Config) DBDSN() string {
	if c.AuthDBDSN != "" {
		return c.AuthDBDSN
	}
	// loc=UTC：MySQL 服务器跑 UTC（与主项目 internal/db 一致，不设 loc 即 UTC）。
	// timeout/readTimeout/writeTimeout：防止 MySQL 卡住时登录请求无限阻塞（无超时会 hang 到前端超时）。
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=UTC&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPassword, c.MySQLHost, c.MySQLPort, c.MySQLDatabase)
}

// CORSOrigins 返回已去除空格的 CORS 源列表（空则 *）。
func (c *Config) CORSOrigins() []string {
	if c.AuthCORSOrigins == "" {
		return []string{"*"}
	}
	parts := strings.Split(c.AuthCORSOrigins, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Load 依次从当前目录、父目录、根目录加载 .env 后解析配置。
// 逐个加载（godotenv.Load 多参数时首个文件缺失即整体失败）；已存在的环境变量优先。
func Load() *Config {
	for _, f := range []string{".env", "../.env", "../../.env"} {
		_ = godotenv.Load(f)
	}
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Printf("[auth-config] env parse note: %v", err)
	}
	return cfg
}
