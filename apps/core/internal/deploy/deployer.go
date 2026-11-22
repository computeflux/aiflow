package deploy

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Hooks 是部署 / 自愈的留痕钩子（S5 §3.2/§3.3：全部动作写 audit_logs，
// 失败写 interventions + 告警）。函数型，避免 deploy → approval/notify 依赖环
// （asset 包已依赖 deploy）。cmd 层接线：approval.Store.Log / interventions 插入 / notify.Notifier。
type Hooks struct {
	// Audit 写审计日志（actor=操作人/deployctl/health-patrol）。
	Audit func(ctx context.Context, actor, action, target string, detail any)
	// Intervene 写干预记录（kind=manual，note=deploy_failed:<step> / self_heal:<action>:<result>）。
	Intervene func(ctx context.Context, target, kind, note string) error
	// Alert 告警（Matrix #ops-alerts，level=info|warning|critical）。
	Alert func(ctx context.Context, title, body, level string)
}

// Deployer 是部署编排器（deployctl deploy / health-patrol 自愈共用）。
type Deployer struct {
	project  *Project
	executor *Executor
	managed  map[string]bool // 纳管资产（compose 服务名）；空 map = 全部
	hooks    Hooks
	actor    string
}

// NewDeployer 创建部署编排器。
func NewDeployer(root, composeDir string, healthTimeoutSec int, managed map[string]bool, hooks Hooks, actor string) (*Deployer, error) {
	project, err := LoadProject(root)
	if err != nil {
		return nil, err
	}
	return &Deployer{
		project:  project,
		executor: NewExecutor(root, composeDir, healthTimeoutSec),
		managed:  managed,
		hooks:    hooks,
		actor:    actor,
	}, nil
}

func (d *Deployer) hookAudit(action, target string, detail any) {
	if d.hooks.Audit != nil {
		d.hooks.Audit(context.Background(), d.actor, action, target, detail)
	}
}

func (d *Deployer) hookAlert(title, body, level string) {
	if d.hooks.Alert != nil {
		d.hooks.Alert(context.Background(), title, body, level)
	}
}

func (d *Deployer) hookIntervene(target, kind, note string) {
	if d.hooks.Intervene != nil {
		if err := d.hooks.Intervene(context.Background(), target, kind, note); err != nil {
			log.Printf("WARN: 写 intervention %s: %v", target, err)
		}
	}
}

// IsManaged 资产是否纳管（空 managed = 全部纳管，本地调试用）。
func (d *Deployer) IsManaged(name string) bool {
	if len(d.managed) == 0 {
		return true
	}
	return d.managed[name]
}

// ManagedServices 返回纳管的非 tools 服务（拓扑序）。
// 一次性容器（restart≠always）仍在集合内：deploy 的 up -d 会启动它们，
// 且按需守护进程（如 sandbox-dind）运行时需被巡检；
// “未启动”的一次性容器在 CheckAsset 里按 skip 处理（不触发自愈，见 health.go）。
func (d *Deployer) ManagedServices() []*Service {
	order, err := d.project.TopoOrder()
	if err != nil {
		log.Printf("WARN: topo order: %v", err)
		return nil
	}
	out := []*Service{}
	for _, name := range order {
		svc := d.project.Services[name]
		if svc.IsTools() || !d.IsManaged(name) {
			continue
		}
		out = append(out, svc)
	}
	return out
}

// CheckAsset 单资产健康检查（与 deployctl 同一套定义，S5 §3.3）。
func (d *Deployer) CheckAsset(ctx context.Context, svc *Service, wait time.Duration) (bool, string) {
	return d.executor.CheckAsset(ctx, svc, wait)
}

// Heal 自愈（S5 §3.3：动作仅限 restart / rebuild+restart / skip，不改配置、不动 schema、不切流量）。
// restartFirst=false 时跳过 restart 直接 rebuild+restart（上一轮 restart 已失败）。
func (d *Deployer) Heal(ctx context.Context, svc *Service, restartFirst bool) (bool, string) {
	if restartFirst {
		if err := d.executor.RestartOne(ctx, svc.Name); err != nil {
			log.Printf("restart %s: %v", svc.Name, err)
		}
		if ok, _ := d.executor.CheckAsset(ctx, svc, 60*time.Second); ok {
			d.hookAudit("self_heal:restart:ok", svc.Name, nil)
			return true, "restart 后恢复"
		}
		d.hookAudit("self_heal:restart:fail", svc.Name, nil)
	}
	// rebuild+restart：给构建一个有界超时（5min）——卡死的构建若不限时，会阻塞整轮巡检的
	// wg.Wait()（S5 踩坑：3.59GB 上下文构建挂起 → 巡检卡死，其余服务不再被检查）。
	buildCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := d.executor.UpOne(buildCtx, svc.Name, true); err != nil {
		log.Printf("rebuild+restart %s: %v", svc.Name, err)
	}
	cancel()
	ok, reason := d.executor.CheckAsset(ctx, svc, 90*time.Second)
	if ok {
		d.hookAudit("self_heal:rebuild:ok", svc.Name, nil)
		return true, "rebuild+restart 后恢复"
	}
	d.hookAudit("self_heal:rebuild:fail", svc.Name, map[string]any{"reason": reason})
	return false, reason
}

// DeployAll 全量部署（S5 §3.2）：
// build（compose 内部并行）→ up -d → 健康检查（拓扑序、并行池）→
// 失败自愈一次（restart → rebuild+restart）→ 仍败即停 + 干预 + 告警（不跳过、不自动改配置）。
func (d *Deployer) DeployAll(ctx context.Context) (*ResultSummary, error) {
	return d.deploy(ctx, "all", nil)
}

// DeployOne 单服务部署（deploy <target>）：build → up -d → 健康检查 → 自愈一次。
func (d *Deployer) DeployOne(ctx context.Context, name string) (*ResultSummary, error) {
	svc, ok := d.project.Services[name]
	if !ok {
		return nil, fmt.Errorf("compose 服务 %q 不存在", name)
	}
	return d.deploy(ctx, name, svc)
}

func (d *Deployer) deploy(ctx context.Context, target string, single *Service) (*ResultSummary, error) {
	start := time.Now()
	sum := &ResultSummary{StartedAt: start}
	d.hookAudit("deploy_start", target, map[string]any{"target": target})

	svcs := d.ManagedServices()
	if single != nil {
		svcs = []*Service{single}
	}

	// 1. build（仅目标范围内带 build 的服务；compose v2 内部并行构建）
	buildStart := time.Now()
	if single != nil {
		if single.Build != nil {
			if err := d.executor.BuildOne(ctx, single.Name); err != nil {
				return d.fail(ctx, sum, "build", single.Name, err)
			}
		}
	} else {
		if err := d.buildScoped(ctx, svcs); err != nil {
			return d.fail(ctx, sum, "build", target, err)
		}
	}
	sum.Steps = append(sum.Steps, PlanStep{
		Target: target, Kind: "build", DurationMs: time.Since(buildStart).Milliseconds(),
		OK:   boolPtr(true),
		Note: "docker compose build（并行）",
	})
	log.Printf("deploy %s: build 完成（%s）", target, time.Since(buildStart))

	// 2. start（up -d；compose 按 depends_on 序启动）
	startStart := time.Now()
	var upErr error
	if single != nil {
		upErr = d.executor.UpOne(ctx, single.Name, false)
	} else {
		upErr = d.executor.UpAll(ctx)
	}
	if upErr != nil {
		return d.fail(ctx, sum, "start", target, upErr)
	}
	sum.Steps = append(sum.Steps, PlanStep{
		Target: target, Kind: "start", DurationMs: time.Since(startStart).Milliseconds(),
		OK:   boolPtr(true),
		Note: "docker compose up -d（depends_on 序）",
	})
	log.Printf("deploy %s: up -d 完成（%s）", target, time.Since(startStart))

	// 3. check（拓扑序、并行池；失败自愈一次）
	checkStart := time.Now()
	failed := map[string]string{}
	d.checkPhase(ctx, svcs, 90*time.Second, failed)
	sum.Steps = append(sum.Steps, PlanStep{
		Target: target, Kind: "check", DurationMs: time.Since(checkStart).Milliseconds(),
		OK:   boolPtr(len(failed) == 0),
		Note: fmt.Sprintf("健康检查 %d 服务，失败 %d", len(svcs), len(failed)),
	})

	sum.FinishedAt = time.Now()
	sum.TotalMs = sum.FinishedAt.Sub(start).Milliseconds()
	if len(failed) > 0 {
		sum.OK = false
		first := firstKey(failed)
		sum.FailedStep = "check:" + first
		sum.FailureNote = failed[first]
		return sum, fmt.Errorf("部署失败（%d 服务不健康）: %s: %s", len(failed), first, failed[first])
	}
	sum.OK = true
	d.hookAudit("deploy_done", target, map[string]any{
		"total_ms": sum.TotalMs, "services": len(svcs),
	})
	d.hookAlert(fmt.Sprintf("部署完成 %s", target),
		fmt.Sprintf("%d 服务全部健康（%s）", len(svcs), time.Since(start).Round(time.Second)), "info")
	return sum, nil
}

// buildScoped 构建目标范围内的服务（deploy all 时 = 全部纳管带 build 的服务）。
func (d *Deployer) buildScoped(ctx context.Context, svcs []*Service) error {
	names := []string{}
	for _, s := range svcs {
		if s.Build != nil {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	log.Printf("build: %d services: %v", len(names), names)
	args := append([]string{"build"}, names...)
	out, err := d.executor.compose(ctx, args...)
	if err != nil {
		log.Printf("build 失败，重试一次: %v", err)
		out2, err2 := d.executor.compose(ctx, args...)
		if err2 != nil {
			return fmt.Errorf("build 重试后仍失败: %v: %s", err2, out2)
		}
		_ = out
	}
	return nil
}

// checkPhase 并行健康检查（池 5）；失败服务自愈一次（restart → rebuild+restart），
// 仍失败记录到 failed（调用方停 + 干预 + 告警）。
func (d *Deployer) checkPhase(ctx context.Context, svcs []*Service, checkTO time.Duration, failed map[string]string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, svc := range svcs {
		wg.Add(1)
		sem <- struct{}{}
		go func(svc *Service) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, reason := d.executor.CheckAsset(ctx, svc, checkTO)
			if !ok {
				log.Printf("check %s 失败（%s），自愈一次", svc.Name, reason)
				d.hookAudit("self_heal", svc.Name, map[string]any{"initial_reason": reason})
				ok, reason = d.Heal(ctx, svc, true)
			}
			if !ok {
				mu.Lock()
				failed[svc.Name] = reason
				mu.Unlock()
				d.hookIntervene(svc.Name, "manual", "deploy_failed:"+svc.Name)
				d.hookAlert("部署失败 "+svc.Name,
					fmt.Sprintf("自愈（restart/rebuild+restart）后仍不健康: %s（不跳过、不自动改配置，请人工介入）", reason), "critical")
			}
		}(svc)
	}
	wg.Wait()
}

// fail 部署步骤失败：停 + 干预 + 告警（不跳过）。
func (d *Deployer) fail(ctx context.Context, sum *ResultSummary, step, target string, err error) (*ResultSummary, error) {
	sum.FinishedAt = time.Now()
	sum.TotalMs = sum.FinishedAt.Sub(sum.StartedAt).Milliseconds()
	sum.OK = false
	sum.FailedStep = step
	sum.FailureNote = err.Error()
	d.hookAudit("deploy_failed", target, map[string]any{"step": step, "error": err.Error()})
	d.hookIntervene(target, "manual", "deploy_failed:"+step)
	d.hookAlert("部署失败 "+target, fmt.Sprintf("步骤 %s: %s（已停，请人工介入）", step, err), "critical")
	return sum, fmt.Errorf("部署 %s 步骤 %s 失败: %w", target, step, err)
}

func boolPtr(b bool) *bool { return &b }

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}
