package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"github.com/wetee/ai-workflow/internal/notify"
)

func (d *driver) ensureEvolveColumns() {
	cols := map[string]string{
		"pr_url":       "ALTER TABLE evolve_tasks ADD COLUMN pr_url VARCHAR(255) DEFAULT ''",
		"pr_error":     "ALTER TABLE evolve_tasks ADD COLUMN pr_error TEXT",
		"merge_commit": "ALTER TABLE evolve_tasks ADD COLUMN merge_commit VARCHAR(64) DEFAULT ''",
		"merge_state":  "ALTER TABLE evolve_tasks ADD COLUMN merge_state VARCHAR(32) DEFAULT ''",
		"session_id":   "ALTER TABLE evolve_tasks ADD COLUMN session_id VARCHAR(128) DEFAULT ''",
		"skip_review":  "ALTER TABLE evolve_tasks ADD COLUMN skip_review TINYINT(1) NOT NULL DEFAULT 0",
		"fail_reason":  "ALTER TABLE evolve_tasks ADD COLUMN fail_reason TEXT",
	}
	for col, ddl := range cols {
		var n int64
		if err := d.db.Raw("SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'evolve_tasks' AND COLUMN_NAME = ?", col).Scan(&n).Error; err != nil {
			log.Printf("WARN: check evolve_tasks.%s: %v", col, err)
			continue
		}
		if n == 0 {
			if err := d.db.Exec(ddl).Error; err != nil {
				log.Printf("WARN: add evolve_tasks.%s: %v", col, err)
				continue
			}
			log.Printf("evolve_tasks: added column %s", col)
		}
	}
	// 文本类错误字段原来是 VARCHAR(255)：错误被截断后看不出根因
	// （典型：只看得见 Dockerfile 片段，看不见 "file not found in build context"）。
	// 统一改成 TEXT；老库需 MODIFY（幂等：只在类型不是 text 时改）。
	for _, col := range []string{"fail_reason", "pr_error"} {
		var dtype string
		if err := d.db.Raw("SELECT DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'evolve_tasks' AND COLUMN_NAME = ?", col).Scan(&dtype).Error; err != nil {
			log.Printf("WARN: check evolve_tasks.%s type: %v", col, err)
			continue
		}
		if dtype == "" || strings.EqualFold(dtype, "text") {
			continue
		}
		if err := d.db.Exec("ALTER TABLE evolve_tasks MODIFY COLUMN " + col + " TEXT").Error; err != nil {
			log.Printf("WARN: widen evolve_tasks.%s: %v", col, err)
			continue
		}
		log.Printf("evolve_tasks: %s widened to TEXT (was %s)", col, dtype)
	}
}

// gitRemoteURL 返回仓库远端 URL（与 api 同口径，人工 PR 链接用）。
func gitRemoteURL() string {
	if v := strings.TrimSpace(os.Getenv("GIT_REMOTE_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "https://github.com/computeflux/workflow"
}

// runCI 起 ci-runner 容器跑全量（go test + uv pytest → JUnit XML 落 ci-cache 卷）。
func (d *driver) runCI(task *db.EvolveTask) (map[string]any, error) {
	// 挂载源必须是**宿主**路径（docker daemon 在宿主侧解析 bind source）
	worktree := strings.TrimSuffix(d.cfg.HostRepo, "/") + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	res, err := d.docker.RunContainer(d.ctx(), dockerapi.ContainerSpec{
		Name:    "ci-" + task.TaskNo,
		Image:   d.cfg.CIRunnerImage,
		Cmd:     []string{"/hacks/ci-runner.sh", task.TaskNo},
		Workdir: "/work",
		// 注：CI 保持 root 运行——它要写 /.cache/go-build（go build 缓存）与 /.crawl4ai（crawl4ai 库
		// 硬编码创建），改成 1001 会直接 PermissionError（2026-10-19 实测）。
		// worktree 不再被污染靠的是 /work 真只读（dockerapi 已修 Mount ReadOnly 字段）。
		// worktree（宿主路径，ro）+ ci-cache 命名卷（JUnit XML / 审核报告共享给审批人）
		Mounts: []dockerapi.Mount{
			{Source: worktree, Destination: "/work", ReadOnly: true},
			{Source: d.cfg.CIVolume, Destination: "/cache/ci", Volume: true},
		},
		NetworkMode: "default",
		Timeout:     30 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("ci-runner: %v", err)
	}
	// 解析 stdout 末行 JSON 摘要
	var out map[string]any
	if err := json.Unmarshal([]byte(lastJSONLine(res.Stdout)), &out); err != nil {
		return nil, fmt.Errorf("ci summary parse: %v: %s", err, tail(res.Stdout, 300))
	}
	d.store.Log(d.ctx(), "evolve-driver", "ci_done", task.TaskNo, out)
	// CI 门槛（§3.8：全绿才进审批）——此前只解析摘要不复核，失败也能建审批单（2026-10-08 实测）
	if err := ciGateError(out); err != nil {
		return nil, fmt.Errorf("%w: %s", err, tail(res.Stdout, 300))
	}
	return out, nil
}

// ciGateError CI 门槛（§3.8：全绿才进审批）：contract != ok（go 构建失败或任何测试失败标记）
// 或 failed>0 → 返回错误（调用方 rejectToHuman）；字段缺失按不通过处理（fail-closed）。
func ciGateError(out map[string]any) error {
	if contract, _ := out["contract"].(string); contract != "ok" {
		return fmt.Errorf("CI contract=%q（构建/测试未全绿）", contract)
	}
	if failed, _ := out["failed"].(float64); failed > 0 {
		return fmt.Errorf("CI 测试失败 %d 个（全绿才进审批）", int(failed))
	}
	return nil
}

// createReleaseApproval 生成审批单（kind=skill_release）并关联进化单。
// image 非空 = CI 后已从 worktree 构建目标镜像（拉灰度时版本容器跑真新代码）；空 = 继承 live。
func (d *driver) createReleaseApproval(task *db.EvolveTask, out *coderOut, testReport map[string]any, image string) error {
	version := d.versionName(task)
	now := time.Now()
	expiresAt := now.Add(time.Duration(d.cfg.ApprovalExpireHours) * time.Hour)
	payload := map[string]any{
		"skill_name":     task.TargetName,
		"version":        version,
		"image":          image,
		"diff":           out.Diff,
		"review":         mustJSONRaw(task.Review),
		"test_report":    testReport,
		"evolve_task_no": task.TaskNo,
		"workdir":        task.Workdir,
	}
	b, _ := json.Marshal(payload)
	// 单号：APR-<task_no>（首次）/ APR-<task_no>-<n>（同单重跑）——任务重试会再生成审批单，
	// 固定单号会撞唯一索引（2026-09-30 实测 1062 Duplicate entry 'APR-EV-20260930-001'）。
	approvalNo := "APR-" + task.TaskNo
	var existing int64
	if err := d.db.Model(&db.Approval{}).
		Where("approval_no = ? OR approval_no LIKE ?", approvalNo, approvalNo+"-%").
		Count(&existing).Error; err == nil && existing > 0 {
		approvalNo = fmt.Sprintf("%s-%d", approvalNo, existing+1)
	}
	apr := &db.Approval{
		ApprovalNo: approvalNo,
		Kind:       "skill_release",
		Subject:    fmt.Sprintf("进化变更 %s v%s（%s）", task.TargetName, version, task.Source),
		Payload:    string(b),
		Status:     "pending",
		Proposer:   "evolve-driver",
		ExpiresAt:  &expiresAt,
		CreatedAt:  now,
	}
	if err := d.db.Create(apr).Error; err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	task.ApprovalID = &apr.ID
	d.setStatus(task, "approving")
	d.store.Log(d.ctx(), "evolve-driver", "approval_created", apr.ApprovalNo, map[string]any{
		"task_no": task.TaskNo, "skill": task.TargetName, "version": version,
	})
	// S4 §3.2：审批单产生 → #approvals（approval.request 协议，diff 摘要 300 字符）
	_ = d.notify.SendApproval(d.ctx(), notify.ApprovalNotice{
		ID: apr.ID, ApprovalNo: apr.ApprovalNo, Kind: "skill_release",
		Subject: apr.Subject, Proposer: "evolve-driver", ExpiresAt: expiresAt,
		Summary: diffSummary(out.Diff),
	})
	return nil
}

// diffSummary 截断 diff 供审批消息展示（全量在 board 审批详情 / ci-cache 卷）。
func diffSummary(diff string) string {
	if len(diff) <= 300 {
		return diff
	}
	return diff[:300] + "…（truncated）"
}

// ---- 状态迁移（全部留痕）

// taskCancelledExternally 判断任务是否已被看板「结束任务」置为 cancelled。
// 驱动器只在轮次边界检查（LLM / sandbox 调用中途无法安全打断）；命中即停止，
// 且 save() 会阻止把它覆盖回运行态。查询失败不误停（fail-open）。
// errTaskFinishedExternally：任务已被外部完成（released/stopped/cancelled/failed）——
// runTask 应当静默退出，不能把它当成失败再改一次状态，更不能继续占着串行队列。
var errTaskFinishedExternally = errors.New("task finished externally")

// taskFinishedExternally 任务是否已经离开「运行态」（被人工结束 / 已合入 / 已失败）。
//
// 2026-11-03 教训：卡片 #176 已经审批合入（released），但它的 reviewer 进程还在跑（单次超时 70 分钟），
// runTask 没退出 → 串行 tick 不领新单 → 后面的 #177 一直停在 open。
func (d *driver) taskFinishedExternally(taskNo string) bool {
	var status string
	if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", taskNo).Pluck("status", &status).Error; err != nil {
		return false
	}
	switch status {
	case "released", "stopped", "cancelled", "failed", "rolled_back", "expired":
		return true
	}
	return false
}

// watchTaskFinished 返回一个 context：任务被外部完成时自动取消（用于杀掉在跑的 coder/reviewer）。
func (d *driver) watchTaskFinished(taskNo string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(d.ctx())
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if d.taskFinishedExternally(taskNo) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (d *driver) taskCancelledExternally(taskNo string) bool {
	var status string
	if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", taskNo).Pluck("status", &status).Error; err != nil {
		return false
	}
	return status == "cancelled"
}

func (d *driver) setStatus(task *db.EvolveTask, status string) {
	task.Status = status
	d.save(task)
	d.store.Log(d.ctx(), "evolve-driver", "task_status_"+status, task.TaskNo, map[string]any{
		"from": "n/a", "target": task.TargetName,
	})
}

func (d *driver) cancel(task *db.EvolveTask, reason string) {
	task.Status = "cancelled"
	d.save(task)
	d.store.Log(d.ctx(), "evolve-driver", "task_cancelled", task.TaskNo,
		map[string]any{"reason": reason, "target": task.TargetName})
	log.Printf("evolve %s: CANCELLED: %s", task.TaskNo, reason)
}

// rejectToHuman 打回转人工（沙箱/CI/审批失败）：interventions(manual) + 告警 + **failed**（不回 Backlog）。
// 2026-11-03 起「失败」不再等价于「丢弃」：卡片留在 In Progress，由人工在看板选
// 「忽略审核」/「续接执行」/「终止」。
func (d *driver) rejectToHuman(task *db.EvolveTask, stage, reason string) {
	_ = d.store.CreateIntervention(d.ctx(), task.TargetName, "manual",
		fmt.Sprintf("进化单 %s 在 %s 阶段打回：%s", task.TaskNo, stage, reason))
	d.notify.SendAlert(d.ctx(), notify.Alert{
		Title: fmt.Sprintf("进化单打回 %s（%s）", task.TaskNo, stage), Body: reason, Level: "critical"})
	d.failTask(task, stage, reason)
}

// failTask 任务失败但**不退回 Backlog**（2026-11-03）：
// 置 status=failed（workitem.Feedback 对 failed 走 default → 工作项保持 in_progress → 卡片留在 In Progress）。
// 与 cancel 的区别：cancel = 人工终止 / 无效工作（卡片回 Backlog）；failed = 可续接的失败。
func (d *driver) failTask(task *db.EvolveTask, stage, reason string) {
	task.Status = "failed"
	task.FailReason = tail(stage+": "+reason, 4000)
	d.save(task)
	d.store.Log(d.ctx(), "evolve-driver", "task_failed", task.TaskNo, map[string]any{
		"stage": stage, "reason": reason, "target": task.TargetName,
	})
	log.Printf("evolve %s: FAILED(%s): %s → 卡片留在 In Progress，等人工选 忽略审核/续接执行/终止",
		task.TaskNo, stage, tail(reason, 200))
}

func (d *driver) save(task *db.EvolveTask) {
	// 外部（看板「结束任务」）已置 cancelled：db.Save 是整行覆盖，绝不能把它写回复运行态。
	// 否则取消会在下一次 save 时被静默“复活”（2026-10-28）。
	if task.Status != "cancelled" && d.taskCancelledExternally(task.TaskNo) {
		task.Status = "cancelled"
	}
	// JSON 列不接受空字符串（MySQL 9.2："The document is empty"）→ 空值写 NULL
	omit := make([]string, 0, 2)
	for col, v := range map[string]string{"review": task.Review, "test_report": task.TestReport} {
		if v == "" {
			omit = append(omit, col)
		}
	}
	if len(omit) == 0 {
		if err := d.db.Save(task).Error; err != nil {
			log.Printf("WARN: save evolve task %s: %v", task.TaskNo, err)
			return
		}
	} else if err := d.db.Omit(omit...).Save(task).Error; err != nil {
		log.Printf("WARN: save evolve task %s: %v", task.TaskNo, err)
		return
	}
	for _, col := range omit {
		if err := d.db.Model(task).UpdateColumn(col, nil).Error; err != nil {
			log.Printf("WARN: nullify %s for %s: %v", col, task.TaskNo, err)
		}
	}
}

// ---- agent 绑定解析（先查 agent_bindings，缺失回退 env，§3.7）

func (d *driver) agent(role, envFallback string) (*db.AgentRegistry, error) {
	var b db.AgentBinding
	if err := d.db.Where("role = ?", role).First(&b).Error; err == nil {
		var a db.AgentRegistry
		if err := d.db.Where("name = ?", b.AgentName).First(&a).Error; err == nil {
			return &a, nil
		}
		log.Printf("WARN: binding %s→%s 的 agent 未注册，回退 env", role, b.AgentName)
	}
	var a db.AgentRegistry
	if err := d.db.Where("name = ?", envFallback).First(&a).Error; err != nil {
		return nil, fmt.Errorf("agent %s（role=%s）未注册（agentctl register 后重试）", envFallback, role)
	}
	return &a, nil
}

// defaultAgentTimeout 未配置 limits.timeout_sec 时的兜底（与 PI_TIMEOUT_SEC 默认一致）。
// 6h：跨服务迁移/大规模重构在本地模型上单轮可能跑数小时；旧值 2h 会把仍在正常推进的
// 任务误杀（2026-11-27 #10，62 次只读调用后被外层 exec 硬超时）。agent 自己会先按内部
// 预算失败，外层只做安全网。
const defaultAgentTimeout = 6 * time.Hour

func agentTimeout(a *db.AgentRegistry) time.Duration {
	if a.Limits == "" {
		return defaultAgentTimeout
	}
	var l struct {
		TimeoutSec int `json:"timeout_sec"`
	}
	if json.Unmarshal([]byte(a.Limits), &l) == nil && l.TimeoutSec > 0 {
		return time.Duration(l.TimeoutSec) * time.Second
	}
	return defaultAgentTimeout
}

// ---- 小工具

func parseEvidence(evidenceJSON string) map[string]any {
	m := map[string]any{}
	if evidenceJSON != "" {
		_ = json.Unmarshal([]byte(evidenceJSON), &m)
	}
	return m
}

func taskText(ev map[string]any) string {
	if s, ok := ev["task"].(string); ok && s != "" {
		return s
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// extractAdapterReason 从 adapter stdout 提取 {"ok":false,"reason":"..."} 的 reason 字段。
// adapter fail() 把结构化错误写 stdout（非 stderr），driver 此前只读 stderr 导致原因丢失。
func extractAdapterReason(stdOut string) string {
	stdOut = strings.TrimSpace(stdOut)
	if stdOut == "" {
		return ""
	}
	var r struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(stdOut), &r) == nil && !r.OK && r.Reason != "" {
		return r.Reason
	}
	return ""
}

func mustJSONRaw(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func lastJSONLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "{") {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

func (c *Config) CacheAbs() string {
	return c.RepoDir + "/" + c.EvolveCacheDir
}

func sandboxImagesFromEnv() []string {
	return sandboximgSplit(os.Getenv("SANDBOX_ALLOWED_IMAGES"))
}
