// Package main is the deployctl binary.
//
// deployctl 是 S5 部署编排器 CLI（build/up/健康检查/自愈/回滚 + 版本容器，docs/plan/S5）。
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"

	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/notify"
)

const usage = `deployctl — S5 部署编排器

用法:
  deployctl inventory                                  刷新 software_assets
  deployctl plan [--save]                              生成依赖序部署计划（可存 deploy_plans）
  deployctl deploy all|<target>                        全量/单服务部署（build→up→健康检查→自愈）
  deployctl status                                     逐资产健康总览
  deployctl rollback <target> [--image <old>]          镜像回滚 / 停止服务
  deployctl version up <skill> <ver> --env test|gray   起测试/灰度版本实例
  deployctl version down <skill> <ver>                 停版本实例
  deployctl version list                               版本实例总览
  deployctl auto-update                                git pull + 重建（替代 computeflux-auto-update）

环境变量:
  MYSQL_*              数据库（required）
  REPO_ROOT            仓库根（默认 /work）
  COMPOSE_DIR          compose 目录（默认 /work/deploy）
  DEPLOY_HEALTH_TIMEOUT  健康探测超时秒（默认 30）
  REPO_HOST_PATH       宿主仓库路径（auto-update 的 git 容器挂载，默认 /home/wetee/work/cathome/workflow）
  DOCKER_HOST          Docker Engine（默认 unix:///var/run/docker.sock）
`

// Config 是本进程的运行时配置（从环境变量 / .env 加载）。
type Config struct {
	MySQLHost     string `env:"MYSQL_HOST,required"`
	MySQLPort     string `env:"MYSQL_PORT,required"`
	MySQLUser     string `env:"MYSQL_USER,required"`
	MySQLPassword string `env:"MYSQL_PASSWORD,required"`
	MySQLDatabase string `env:"MYSQL_DATABASE,required"`

	RepoRoot      string `env:"REPO_ROOT" envDefault:"/work"`
	ComposeDir    string `env:"COMPOSE_DIR" envDefault:"/work/deploy"`
	HealthTimeout int    `env:"DEPLOY_HEALTH_TIMEOUT" envDefault:"30"`
	RepoHostPath  string `env:"REPO_HOST_PATH" envDefault:"/home/wetee/work/cathome/workflow"`
	DockerHost    string `env:"DOCKER_HOST" envDefault:"unix:///var/run/docker.sock"`
	Actor         string `env:"DEPLOY_ACTOR" envDefault:"deployctl"`
}

func main() {
	_ = godotenv.Load()
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		fatalf("config: %v", err)
	}
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(1)
	}

	database, dbErr := openDB(cfg)
	var store *deploy.Store
	if dbErr != nil {
		// DB 不可用（如 docker compose down 后的恢复场景）：降级运行——
		// 纳管集用 compose 兜底，留痕（audit/interventions/deploy_plans）跳过。不 fatal。
		log.Printf("WARN: 打开 MySQL 失败（%v）；降级运行（纳管集用 compose 兜底，跳过留痕）", dbErr)
		database = nil
	} else {
		store = deploy.NewStore(database)
		if err := store.EnsureSchema(); err != nil {
			fatalf("ensure schema: %v", err)
		}
		if err := deploy.EnsureOpsTables(database); err != nil {
			fatalf("ensure ops tables: %v", err)
		}
	}
	notifier := notify.New()

	switch args[0] {
	case "inventory":
		cmdInventory(cfg, database, store)
	case "plan":
		cmdPlan(cfg, database, store, len(args) > 1 && args[1] == "--save")
	case "deploy":
		if len(args) < 2 {
			fatalf("用法: deployctl deploy all|<target>")
		}
		cmdDeploy(cfg, database, store, notifier, args[1])
	case "status":
		cmdStatus(cfg, database, store)
	case "rollback":
		if len(args) < 2 {
			fatalf("用法: deployctl rollback <target> [--image <old>]")
		}
		cmdRollback(cfg, database, notifier, args[1:])
	case "version":
		cmdVersion(cfg, database, store, notifier, args[1:])
	case "auto-update":
		cmdAutoUpdate(cfg, database, store, notifier)
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}
