package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/deploy"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"github.com/wetee/ai-workflow/internal/notify"
	"gorm.io/gorm"
)

// cmds.go：deployctl 子命令实现（从 main.go 拆出，纯机械搬迁）。

// cmdInventory 刷新 software_assets（与 cmd/inventory 同一逻辑）。
func cmdInventory(cfg *Config, database *gorm.DB, store *deploy.Store) {
	ctx := context.Background()
	if store == nil {
		fatalf("inventory 需要 MySQL（当前不可用；请先启动 infra/mysql 再刷新画像）")
	}
	scanner := deploy.NewScanner(cfg.RepoRoot, database)
	assets, err := scanner.Scan(ctx)
	if err != nil {
		fatalf("scan: %v", err)
	}
	if err := store.UpsertAll(ctx, assets); err != nil {
		fatalf("upsert: %v", err)
	}
	audit(ctx, database, cfg.Actor, "inventory_refresh", "software_assets", map[string]any{"count": len(assets)})
	managed, external := 0, 0
	for _, a := range assets {
		if a.AutoManaged == 1 {
			managed++
		} else {
			external++
		}
	}
	fmt.Printf("software_assets: %d 资产（纳管 %d / 不纳管 %d）\n", len(assets), managed, external)
}

// cmdPlan 生成依赖序部署计划（拓扑排序）；--save 存 deploy_plans。
// managedSet 读取纳管集；DB 不可用（如 docker compose down 后）时兜底为 compose 派生
// 的默认集（排除 external 红线）——S5「一条命令恢复」依赖它，否则 deploy all 在 DB
// 未启动时无法读取纳管清单。
func managedSet(ctx context.Context, store *deploy.Store, project *deploy.Project) map[string]bool {
	if store != nil {
		if m, err := store.ManagedComposeServices(ctx); err == nil {
			return m
		}
	}
	log.Printf("WARN: 读 software_assets 失败（DB 不可用？），兜底为 compose 默认纳管集（排除 external 红线）")
	return deploy.DefaultManagedSet(project)
}

func cmdPlan(cfg *Config, database *gorm.DB, store *deploy.Store, save bool) {
	ctx := context.Background()
	project, err := deploy.LoadProject(cfg.RepoRoot)
	if err != nil {
		fatalf("load compose: %v", err)
	}
	managed := managedSet(ctx, store, project)
	plan, err := deploy.GeneratePlan(project, managed, cfg.HealthTimeout)
	if err != nil {
		fatalf("generate plan: %v", err)
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	fmt.Println(string(b))
	if save {
		if store == nil {
			fatalf("save plan 需要 MySQL（当前不可用）")
		}
		if err := store.SavePlan(ctx, &deploy.DeployPlan{Name: "default", Plan: string(b), Status: "idle"}); err != nil {
			fatalf("save plan: %v", err)
		}
		audit(ctx, database, cfg.Actor, "plan_save", "deploy_plans:default", map[string]any{"steps": len(plan.Steps)})
		fmt.Printf("saved to deploy_plans (name=default, %d steps)\n", len(plan.Steps))
	}
}

// cmdDeploy 部署（all / <target>）：build（并行）→ up -d → 健康检查 → 自愈一次 → 仍败即停+干预+告警。
func cmdDeploy(cfg *Config, database *gorm.DB, store *deploy.Store, notifier notify.Notifier, target string) {
	ctx := context.Background()
	project, err := deploy.LoadProject(cfg.RepoRoot)
	if err != nil {
		fatalf("load compose: %v", err)
	}
	managed := managedSet(ctx, store, project)
	depl, err := deploy.NewDeployer(cfg.RepoRoot, cfg.ComposeDir, cfg.HealthTimeout, managed, hooks(cfg, database, notifier), cfg.Actor)
	if err != nil {
		fatalf("new deployer: %v", err)
	}
	var sum *deploy.ResultSummary
	if target == "all" {
		sum, err = depl.DeployAll(ctx)
	} else {
		sum, err = depl.DeployOne(ctx, target)
	}
	if err != nil {
		if sum != nil {
			recordPlanRun(ctx, store, cfg, sum, "failed")
		}
		fatalf("%v", err)
	}
	recordPlanRun(ctx, store, cfg, sum, "done")
	fmt.Printf("deploy %s OK: %d ms\n", target, sum.TotalMs)
}

// cmdStatus 逐资产健康总览（docker compose ps --format json + 每资产健康判定）。
func cmdStatus(cfg *Config, database *gorm.DB, store *deploy.Store) {
	ctx := context.Background()
	project, err := deploy.LoadProject(cfg.RepoRoot)
	if err != nil {
		fatalf("load compose: %v", err)
	}
	managed := managedSet(ctx, store, project)
	exec := deploy.NewExecutor(cfg.RepoRoot, cfg.ComposeDir, cfg.HealthTimeout)
	ps, err := exec.PS(ctx)
	if err != nil {
		fatalf("compose ps: %v", err)
	}
	order, _ := project.TopoOrder()
	green, yellow, red := 0, 0, 0
	for _, name := range order {
		svc := project.Services[name]
		if svc.IsTools() {
			continue
		}
		st, ok := ps[name]
		mark, state := "✖", "not created"
		if !ok && svc.OneShot {
			// 一次性容器未启动（init 已完成 / 按需守护进程已停）：合法状态，不告警（与自愈 skip 一致）
			mark, state = "◐", "not started（一次性，跳过）"
		}
		if ok {
			switch {
			case svc.OneShot && st.State == "exited" && st.ExitCode == 0:
				mark, state = "✔", "exited(0)"
			case st.State == "running" && st.Health == "healthy":
				mark, state = "✔", "healthy"
			case st.State == "running" && st.Health == "starting":
				mark, state = "◐", "starting"
			case st.State == "running" && st.Health == "unhealthy":
				mark, state = "✖", "unhealthy"
			case st.State == "running":
				mark, state = "◐", "running（无 healthcheck）"
			default:
				mark, state = "✖", fmt.Sprintf("%s（%s）", st.State, st.Status)
			}
		}
		switch mark {
		case "✔":
			green++
		case "◐":
			yellow++
		default:
			red++
		}
		managedMark := " "
		if len(managed) == 0 || managed[name] {
			managedMark = "M"
		}
		fmt.Printf("  %s %s %-24s %s\n", mark, managedMark, name, state)
	}
	fmt.Printf("总览: %d 绿 / %d 黄 / %d 红（M=纳管）\n", green, yellow, red)
	if red > 0 {
		os.Exit(2)
	}
}

// cmdRollback 回滚：<target> 无 --image = docker compose stop；
// 带 --image <old> = 临时 override（/tmp，不落仓库）换旧 tag 镜像重建该服务。
func cmdRollback(cfg *Config, database *gorm.DB, notifier notify.Notifier, args []string) {
	ctx := context.Background()
	target := args[0]
	oldImage := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--image" && i+1 < len(args) {
			oldImage = args[i+1]
		}
	}
	project, err := deploy.LoadProject(cfg.RepoRoot)
	if err != nil {
		fatalf("load compose: %v", err)
	}
	if _, ok := project.Services[target]; !ok {
		fatalf("compose 服务 %q 不存在", target)
	}
	exec := deploy.NewExecutor(cfg.RepoRoot, cfg.ComposeDir, cfg.HealthTimeout)
	hooksInst := hooks(cfg, database, notifier)

	if oldImage == "" {
		if err := exec.StopOne(ctx, target); err != nil {
			fatalf("%v", err)
		}
		hooksInst.Intervene(ctx, target, "manual", "rollback_stopped:"+target)
		audit(ctx, database, cfg.Actor, "rollback_stop", target, nil)
		fmt.Printf("stopped %s（镜像与数据保留，docker compose up -d %s 可再启动）\n", target, target)
		return
	}
	if err := exec.RollbackImage(ctx, target, oldImage); err != nil {
		fatalf("%v", err)
	}
	hooksInst.Intervene(ctx, target, "manual", "rollback_image:"+target+"→"+oldImage)
	audit(ctx, database, cfg.Actor, "rollback_image", target, map[string]any{"image": oldImage})
	notifier.SendAlert(ctx, notify.Alert{
		Title: fmt.Sprintf("镜像回滚 %s", target),
		Body:  fmt.Sprintf("%s 已切到旧镜像 %s", target, oldImage),
		Level: "warning",
	})
	fmt.Printf("rolled back %s → %s\n", target, oldImage)
}

// cmdVersion 版本容器（§3.4）：up / down / list。
func cmdVersion(cfg *Config, database *gorm.DB, store *deploy.Store, notifier notify.Notifier, args []string) {
	if len(args) == 0 {
		fatalf("用法: deployctl version up|down|list ...")
	}
	ctx := context.Background()
	docker, err := dockerapi.New(cfg.DockerHost)
	if err != nil {
		fatalf("docker client: %v", err)
	}
	project, err := deploy.LoadProject(cfg.RepoRoot)
	if err != nil {
		fatalf("load compose: %v", err)
	}
	vm := deploy.NewVersionManager(database, docker, project, cfg.RepoRoot)

	switch args[0] {
	case "up":
		if len(args) < 3 {
			fatalf("用法: deployctl version up <skill> <ver> --env test|gray")
		}
		env := "test"
		for i := 2; i < len(args); i++ {
			if args[i] == "--env" && i+1 < len(args) {
				env = args[i+1]
			}
		}
		if env != "test" && env != "gray" {
			fatalf("--env 须为 test 或 gray")
		}
		addr, err := vm.Up(ctx, args[1], args[2], env)
		if err != nil {
			fatalf("version up: %v", err)
		}
		audit(ctx, database, cfg.Actor, "version_up", fmt.Sprintf("%s v%s", args[1], args[2]), map[string]any{"env": env, "addr": addr})
		notifier.SendAlert(ctx, notify.Alert{
			Title: fmt.Sprintf("版本实例已起 %s v%s（%s）", args[1], args[2], env),
			Body:  addr,
			Level: "info",
		})
		fmt.Printf("version up OK: %s v%s (%s) → %s\n", args[1], args[2], env, addr)
	case "down":
		if len(args) < 3 {
			fatalf("用法: deployctl version down <skill> <ver>")
		}
		if err := vm.Down(ctx, args[1], args[2]); err != nil {
			fatalf("version down: %v", err)
		}
		audit(ctx, database, cfg.Actor, "version_down", fmt.Sprintf("%s v%s", args[1], args[2]), nil)
		notifier.SendAlert(ctx, notify.Alert{
			Title: fmt.Sprintf("版本实例已停 %s v%s", args[1], args[2]),
			Body:  "分流清零；status=retired/draft",
			Level: "info",
		})
		fmt.Printf("version down OK: %s v%s\n", args[1], args[2])
	case "list":
		var versions []db.SkillVersion
		if err := database.Order("skill_id, id DESC").Find(&versions).Error; err != nil {
			fatalf("list versions: %v", err)
		}
		var skills []db.Skill
		if err := database.Find(&skills).Error; err != nil {
			fatalf("list skills: %v", err)
		}
		byID := map[uint]string{}
		for _, s := range skills {
			byID[s.ID] = s.Name
		}
		fmt.Printf("%-16s %-10s %-28s %-8s %-10s\n", "SKILL", "VERSION", "IMAGE", "STATUS", "TRAFFIC")
		for _, v := range versions {
			fmt.Printf("%-16s %-10s %-28s %-8s %-10d\n", byID[v.SkillID], v.Version, v.Image, v.Status, v.TrafficSplit)
		}
	default:
		fatalf("用法: deployctl version up|down|list ...")
	}
}

// cmdAutoUpdate 替代原 computeflux-auto-update：git pull（一次性 git 容器）+ 重建 + 健康检查。
func cmdAutoUpdate(cfg *Config, database *gorm.DB, store *deploy.Store, notifier notify.Notifier) {
	ctx := context.Background()
	docker, err := dockerapi.New(cfg.DockerHost)
	if err != nil {
		fatalf("docker client: %v", err)
	}
	// distroless 无 git：一次性 alpine/git 容器挂载宿主仓库路径
	res, err := docker.RunContainer(ctx, dockerapi.ContainerSpec{
		Name:    "deployctl-git-pull",
		Image:   "alpine/git:latest",
		Cmd:     []string{"sh", "-c", "git -C /work pull --ff-only origin main"},
		Workdir: "/work",
		Mounts:  []dockerapi.Mount{{Source: cfg.RepoHostPath, Destination: "/work"}},
		Timeout: 5 * time.Minute,
	})
	if err != nil {
		fatalf("git pull: %v", err)
	}
	pulled := !strings.Contains(res.Stdout, "Already up to date")
	audit(ctx, database, cfg.Actor, "auto_update", "repo", map[string]any{"pulled": pulled, "sha": strings.TrimSpace(res.Stdout)})
	if !pulled {
		fmt.Println("already up to date, 无变更")
		return
	}
	fmt.Println("new commit pulled, 重建全系统 ...")
	managed, err := store.ManagedComposeServices(ctx)
	if err != nil {
		fatalf("list managed: %v", err)
	}
	depl, err := deploy.NewDeployer(cfg.RepoRoot, cfg.ComposeDir, cfg.HealthTimeout, managed, hooks(cfg, database, notifier), cfg.Actor)
	if err != nil {
		fatalf("new deployer: %v", err)
	}
	sum, err := depl.DeployAll(ctx)
	if err != nil {
		fatalf("deploy after auto-update: %v", err)
	}
	recordPlanRun(ctx, store, cfg, sum, "done")
	fmt.Printf("auto-update 完成（%d ms）\n", sum.TotalMs)
}

// recordPlanRun 写 deploy_plans 执行结果（name=default）。store 为 nil（DB 降级）时跳过。
func recordPlanRun(ctx context.Context, store *deploy.Store, cfg *Config, sum *deploy.ResultSummary, status string) {
	if store == nil {
		return
	}
	if err := store.RecordPlanRun(ctx, "default", status, sum); err != nil {
		// plan 未保存时不阻断（status 命令不依赖它）
		log.Printf("WARN: record plan run: %v", err)
	}
}
