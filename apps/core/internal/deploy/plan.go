package deploy

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

func sortStrings(s []string) { sort.Strings(s) }

// PlanStep 是部署计划的一步（S5 §3.2 deploy_plans.plan 数组元素）。
type PlanStep struct {
	Target     string      `json:"target"`
	Kind       string      `json:"kind"` // build | start | check
	DependsOn  []string    `json:"depends_on,omitempty"`
	Health     *HealthSpec `json:"health,omitempty"`
	Action     *ActionSpec `json:"action,omitempty"`
	OneShot    bool        `json:"one_shot,omitempty"`
	Resources  *Resources  `json:"resources,omitempty"`
	DurationMs int64       `json:"duration_ms,omitempty"` // 执行后回填（报告可回溯每步耗时）
	OK         *bool       `json:"ok,omitempty"`          // 执行后回填
	Note       string      `json:"note,omitempty"`
}

// HealthSpec 是每资产健康定义（deployctl 与 health-patrol 共用）。
type HealthSpec struct {
	URL        string `json:"url,omitempty"`
	ExpectCode int    `json:"expect_code"`
	TimeoutSec int    `json:"timeout_sec"`
}

// ActionSpec 是每步动作命令（报告用；实际执行走 Executor 方法）。
type ActionSpec struct {
	BuildCmd   string `json:"build_cmd,omitempty"`
	StartCmd   string `json:"start_cmd,omitempty"`
	RestartCmd string `json:"restart_cmd,omitempty"`
	RebuildCmd string `json:"rebuild_cmd,omitempty"`
	StopCmd    string `json:"stop_cmd,omitempty"`
}

// Resources 是资源限额（版本容器与生产一致，S5 §3.4）。
type Resources struct {
	MemLimit string `json:"mem_limit,omitempty"`
	CPUs     string `json:"cpus,omitempty"`
}

// Plan 是完整部署计划（拓扑序）。
type Plan struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Steps       []PlanStep `json:"steps"`
}

// GeneratePlan 生成依赖序部署计划（拓扑排序；与 compose depends_on 一致，
// 基线序 infra → core → skills → evolve → tools 由 TopoOrder 的 include 文件序保证）。
//
// 三类步骤：
//  1. build（带 build 定义的服务）；
//  2. start（全部服务，up -d 由 compose 按 depends_on 序启动）；
//  3. check（健康检查；纳管资产才检查，external 资产零触碰）。
func GeneratePlan(p *Project, managed map[string]bool, healthTimeoutSec int) (*Plan, error) {
	order, err := p.TopoOrder()
	if err != nil {
		return nil, err
	}
	if healthTimeoutSec <= 0 {
		healthTimeoutSec = 30
	}
	plan := &Plan{GeneratedAt: time.Now()}
	for _, name := range order {
		svc := p.Services[name]
		if svc.IsTools() {
			continue // tools profile 一次性工具不进部署计划
		}
		isManaged := managed[name] // managed 为空 map 时 = 全部纳管（deployctl 本地调试）
		if len(managed) > 0 && !isManaged {
			continue // external 资产：计划中不出现（零触碰红线）
		}

		// 1. build
		if svc.Build != nil {
			plan.Steps = append(plan.Steps, PlanStep{
				Target:    name,
				Kind:      "build",
				DependsOn: depNames(svc),
				Action: &ActionSpec{
					BuildCmd:   fmt.Sprintf("docker compose build %s", name),
					StartCmd:   fmt.Sprintf("docker compose up -d %s", name),
					RestartCmd: fmt.Sprintf("docker compose restart %s", name),
					RebuildCmd: fmt.Sprintf("docker compose up -d --build %s", name),
					StopCmd:    fmt.Sprintf("docker compose stop %s", name),
				},
				Resources: resourcesOf(svc),
			})
		}
		// 2. start
		plan.Steps = append(plan.Steps, PlanStep{
			Target:    name,
			Kind:      "start",
			DependsOn: depNames(svc),
			Action: &ActionSpec{
				StartCmd:   fmt.Sprintf("docker compose up -d %s", name),
				RestartCmd: fmt.Sprintf("docker compose restart %s", name),
				RebuildCmd: fmt.Sprintf("docker compose up -d --build %s", name),
				StopCmd:    fmt.Sprintf("docker compose stop %s", name),
			},
			OneShot:   svc.OneShot,
			Resources: resourcesOf(svc),
		})
		// 3. check
		health := &HealthSpec{ExpectCode: 200, TimeoutSec: healthTimeoutSec}
		if svc.HealthURL != "" {
			health.URL = svc.HealthURL
		}
		plan.Steps = append(plan.Steps, PlanStep{
			Target:    name,
			Kind:      "check",
			DependsOn: depNames(svc),
			Health:    health,
			OneShot:   svc.OneShot,
		})
	}
	return plan, nil
}

// depNames 返回 depends_on 服务名（排序，确定性）。
func depNames(svc *Service) []string {
	out := make([]string, 0, len(svc.DependsOn))
	for d := range svc.DependsOn {
		out = append(out, d)
	}
	sortStrings(out)
	return out
}

func resourcesOf(svc *Service) *Resources {
	if svc.MemLimit == "" && svc.CPUs == "" {
		return nil
	}
	return &Resources{MemLimit: svc.MemLimit, CPUs: svc.CPUs}
}

// MarshalJSON 序列化计划（存 deploy_plans.plan）。
func (p *Plan) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		GeneratedAt time.Time  `json:"generated_at"`
		Steps       []PlanStep `json:"steps"`
	}{p.GeneratedAt, p.Steps})
}

// UnmarshalPlan 解析 deploy_plans.plan 列。
func UnmarshalPlan(raw string) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("parse deploy plan: %w", err)
	}
	return &p, nil
}

// ResultSummary 是 deploy 执行结果（写 deploy_plans.last_result）。
type ResultSummary struct {
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  time.Time  `json:"finished_at"`
	TotalMs     int64      `json:"total_ms"`
	OK          bool       `json:"ok"`
	FailedStep  string     `json:"failed_step,omitempty"`
	FailureNote string     `json:"failure_note,omitempty"`
	Steps       []PlanStep `json:"steps"`
}
