package config

import (
	"log"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	AppHost string `env:"APP_HOST" envDefault:"0.0.0.0"`
	AppPort string `env:"MAILER_PORT" envDefault:"8003"`

	MySQLHost     string `env:"MYSQL_HOST" envDefault:"127.0.0.1"`
	MySQLPort     string `env:"MYSQL_PORT" envDefault:"3306"`
	MySQLUser     string `env:"MYSQL_USER" envDefault:"root"`
	MySQLPassword string `env:"MYSQL_PASSWORD"`
	MySQLDatabase string `env:"MYSQL_DATABASE" envDefault:"ai_workflow"`

	TemporalHostPort  string `env:"TEMPORAL_HOST_PORT" envDefault:"localhost:7233"`
	TemporalNamespace string `env:"TEMPORAL_NAMESPACE" envDefault:"default"`
	TemporalTaskQueue string `env:"TEMPORAL_GO_TASK_QUEUE" envDefault:"go-workflow-taskqueue"`

	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"465"`
	SMTPUser     string `env:"SMTP_USER"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPFrom     string `env:"SMTP_FROM"`
}

func Load() *Config {
	// 尝试依次从当前目录、父目录、根目录加载 .env
	_ = godotenv.Load(".env", "../.env", "../../.env")
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		log.Printf("[mailer-config] env parse note: %v", err)
	}
	return cfg
}
