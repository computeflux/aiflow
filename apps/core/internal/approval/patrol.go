package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// patrolApproval 是 audit 巡检生成严重问题时落库的审批单 + 进化单（§3.3）：
// 只生成审批单（kind=rollback）与 evolve_tasks（source=patrol），**不自动改流量**；
// 同一 skill 已有 pending 的 rollback 审批单则跳过（去重，避免巡检重复开单）。

// PatrolFinding 是巡检发现（audit 报告 findings 的审计留痕子集）。
type PatrolFinding struct {
	Severity string         `json:"severity"`
	Metric   string         `json:"metric"`
	Value    string         `json:"value"`
	Evidence map[string]any `json:"evidence"`
}

// CreatePatrolApproval 为严重问题 skill 生成 rollback 审批单（+ 可选 patrol 进化单）。
// createEvolveTask=false 时只建审批单、不建 evolve_task（S9 §7.6：改由工作项驱动，见 cmd/audit）。
// 返回是否新建（false = 已存在 pending 审批单，跳过）。
func (s *Store) CreatePatrolApproval(ctx context.Context, skillName string, findings []PatrolFinding, reportFile string, expireHours int, createEvolveTask bool) (bool, error) {
	now := time.Now()

	// 去重：同 skill 已有 pending 的 rollback 审批单
	var dup int64
	err := s.db.WithContext(ctx).Model(&db.Approval{}).
		Where("kind = ? AND status = ? AND JSON_UNQUOTE(JSON_EXTRACT(payload, '$.skill_name')) = ?",
			"rollback", "pending", skillName).
		Count(&dup).Error
	if err != nil {
		return false, fmt.Errorf("dedup check: %w", err)
	}
	if dup > 0 {
		s.Log(ctx, "audit", "patrol_approval_deduped", skillName,
			map[string]any{"note": "已有 pending rollback 审批单，跳过"})
		return false, nil
	}

	// 当前 live 版本（回滚目标）
	var liveVersion string
	var skill db.Skill
	if err := s.db.WithContext(ctx).Where("name = ?", skillName).First(&skill).Error; err == nil {
		var v db.SkillVersion
		if err := s.db.WithContext(ctx).
			Where("skill_id = ? AND status = ? AND traffic_split > 0 ORDER BY traffic_split DESC", skill.ID, "live").
			First(&v).Error; err == nil {
			liveVersion = v.Version
		}
	}

	payload := map[string]any{
		"skill_name":   skillName,
		"version":      liveVersion,
		"report":       reportFile,
		"findings":     findings,
		"generated_at": now.Format(time.RFC3339),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	expiresAt := now.Add(time.Duration(expireHours) * time.Hour)
	// approval_no 列是 varchar(32)：秒级时间戳 + 4 位 hex 后缀（skill 名在 subject/payload，不进编号）；
	// 旧格式 APR-PAT-<14位>-<12位名> 超 32 字符会 1406（2026-10-02 S4 E2E 实测）。
	approvalNo := fmt.Sprintf("APR-PAT-%d-%s", now.Unix(), randHex4())
	approval := &db.Approval{
		ApprovalNo: approvalNo,
		Kind:       "rollback",
		Subject:    fmt.Sprintf("巡检告警：%s 指标超阈值（建议回滚评估）", skillName),
		Payload:    string(b),
		Status:     "pending",
		Proposer:   "audit",
		ExpiresAt:  &expiresAt,
		CreatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Create(approval).Error; err != nil {
		return false, fmt.Errorf("create patrol approval: %w", err)
	}
	s.Log(ctx, "audit", "patrol_approval_created", approval.ApprovalNo,
		map[string]any{"skill_name": skillName, "live_version": liveVersion, "report": reportFile})
	// S4 §3.2：审批单产生 → #approvals（approval.request 协议）
	_ = s.notify.SendApproval(ctx, notify.ApprovalNotice{
		ID: approval.ID, ApprovalNo: approval.ApprovalNo, Kind: "rollback",
		Subject: approval.Subject, Proposer: "audit", ExpiresAt: expiresAt,
		Summary: reportFile,
	})

	// 进化单（source=patrol，status=open → evolve-driver 拾取）；S9 下可关闭（改由工作项驱动）。
	if createEvolveTask {
		evidence := map[string]any{
			"task": fmt.Sprintf("巡检发现 %s 指标超阈值（审批单 %s），请分析并修复：\n%s",
				skillName, approval.ApprovalNo, mustJSONString(findings)),
			"report":      reportFile,
			"approval_no": approval.ApprovalNo,
		}
		var seq int64
		_ = s.db.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("task_no LIKE ?", "EV-"+now.Format("20060102")+"-%").
			Count(&seq).Error
		task := &db.EvolveTask{
			TaskNo:     fmt.Sprintf("EV-%s-%03d", now.Format("20060102"), seq+1),
			Source:     "patrol",
			Evidence:   mustJSONString(evidence),
			TargetName: skillName,
			Status:     "open",
			CreatedAt:  now,
		}
		if err := s.db.WithContext(ctx).Create(task).Error; err != nil {
			return false, fmt.Errorf("create patrol evolve task: %w", err)
		}
		s.Log(ctx, "audit", "patrol_evolve_task_created", task.TaskNo,
			map[string]any{"skill_name": skillName, "approval_id": approval.ID})
	}
	return true, nil
}

// randHex4 审批单号 4 位 hex 后缀（唯一性兜底，撞了会 1062 由上层报错）。
func randHex4() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mustJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
