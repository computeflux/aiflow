// Package config 加载并校验全系统运行时配置（根 .env 为单一事实来源）。
package config

import (
	"log"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	// Host/port are fine with defaults
	AppHost string `env:"APP_HOST" envDefault:"0.0.0.0"`
	AppPort string `env:"APP_PORT" envDefault:"8000"`

	// All credentials and service addresses MUST come from .env — fail if missing
	TemporalHostPort          string `env:"TEMPORAL_HOST_PORT,required"`
	TemporalNamespace         string `env:"TEMPORAL_NAMESPACE,required"`
	TemporalGoTaskQueue       string `env:"TEMPORAL_GO_TASK_QUEUE,required"`
	TemporalPythonTaskQueue   string `env:"TEMPORAL_PYTHON_TASK_QUEUE,required"`
	TemporalPyreplayTaskQueue string `env:"TEMPORAL_PYREPLAY_TASK_QUEUE" envDefault:"pyreplay-task-queue"`

	// S2 混合编排（docs/plan/S2-混合编排.md §3.1）：agent-prod namespace 三队列。
	// 默认值即计划值，.env 可覆盖；CRON_QUEUE_MIGRATED 控制 cron workflow 是否
	// 迁到 agent-prod 的 cron 队列（双跑一个版本周期后停 default 旧注册，§7）。
	TemporalAgentNamespace string `env:"TEMPORAL_AGENT_NAMESPACE" envDefault:"agent-prod"`
	SkillTaskQueue         string `env:"SKILL_TASK_QUEUE" envDefault:"skill-queue"`
	FlexTaskQueue          string `env:"FLEX_TASK_QUEUE" envDefault:"flex-queue"`
	CronTaskQueue          string `env:"CRON_TASK_QUEUE" envDefault:"cron-queue"`
	CronQueueMigrated      bool   `env:"CRON_QUEUE_MIGRATED" envDefault:"false"`
	// S2 柔性分支（§3.3）：flex-server（Go 自研图引擎，替代 LangGraph 选型）内部端点（仅内部网络，不发布端口）
	FlexEndpoint string `env:"FLEX_ENDPOINT" envDefault:"http://flex-server:8124"`
	// S2 §3.2：WeeklyContentWorkflow 审批等待超时（小时）；验收超时路径时临时调小（如 1）
	WeeklyApprovalWaitHours int `env:"WEEKLY_APPROVAL_WAIT_HOURS" envDefault:"24"`

	// 订阅目标点赞分发 cron（pyreplay FollowActivities.dispatch_likes）
	FollowLikeDispatchCron string `env:"FOLLOW_LIKE_DISPATCH_CRON" envDefault:"*/15 * * * *"`

	QdrantHost       string `env:"QDRANT_HOST,required"`
	QdrantGRPCPort   int    `env:"QDRANT_GRPC_PORT,required"`
	QdrantCollection string `env:"QDRANT_COLLECTION,required"`

	OpenAIAPIKey  string `env:"OPENAI_API_KEY,required"`
	OpenAIBaseURL string `env:"OPENAI_BASE_URL,required"`
	// 嵌入模型当前不可用（模型列表中已移除），临时停用：留空时
	// Embed/EmbedBatch 不再调用外部 API，向量索引与知识库搜索自动跳过。
	EmbeddingModel string `env:"EMBEDDING_MODEL" envDefault:""`
	LLMModel       string `env:"LLM_MODEL,required"`

	// MySQL 采用和 Python worker 一致的独立变量，内部拼 DSN
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	IngestCron   string `env:"INGEST_CRON,required"`
	GenerateCron string `env:"GENERATE_CRON,required"`

	SMTPHost     string `env:"SMTP_HOST,required"`
	SMTPPort     int    `env:"SMTP_PORT,required"`
	SMTPUser     string `env:"SMTP_USER,required"`
	SMTPPassword string `env:"SMTP_PASSWORD,required"`
	SMTPFrom     string `env:"SMTP_FROM,required"`

	// 统一认证（apps/auth）：本地 JWKS 验签。AUTH_URL 为空时不启用鉴权中间件（兼容未部署 auth 的环境）。
	AuthURL      string `env:"AUTH_URL" envDefault:"http://127.0.0.1:8008"`
	AuthIssuer   string `env:"AUTH_ISSUER" envDefault:"https://auth.computeflux.ai"`
	AuthAudience string `env:"AUTH_AUDIENCE" envDefault:"workflow"`
}

// Load parses environment variables into Config.
func Load() *Config {
	_ = godotenv.Load()

	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	return cfg
}
