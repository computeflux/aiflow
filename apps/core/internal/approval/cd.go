package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/project"
	"gorm.io/gorm"
)

// cdPayload 是审批单 payload 里 CD 需要的字段
// （skill_release 由 evolve-driver 生成；rollback 由 audit 巡检生成；
// workflow_signal 由 weekly-content 触发 API 生成，S2 §3.2）。
type cdPayload struct {
	SkillName  string `json:"skill_name"`
	Version    string `json:"version"`
	Image      string `json:"image"` // S1 evolve：CI 后从 worktree 构建的目标镜像（空 = 继承 live 旧镜像）
	RollbackTo string `json:"rollback_to"`
	// S2 §3.2：workflow 审批单（kind=workflow_signal）
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

// RunCD 在人工 approve 后执行 CD（docs/plan/S1-进化闭环.md §3.2）。
// 每次审批动作都写 audit_logs（不可删）。
//   - skill_release：新版本 draft→gray（traffic=5），旧 live 保持 95%，
//     之后由 TickGray 自动推进 5→50→100（§3.4）；
//   - rollback：live 版本切回 rollback_to 指向的版本；
//   - takeover：S3 接管舱，S1 仅留痕。
func (s *Store) RunCD(ctx context.Context, a *db.Approval) error {
	var p cdPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		s.Log(ctx, "cd", "payload_parse_failed", a.ApprovalNo, ginErr(a, err))
		return fmt.Errorf("parse approval payload: %w", err)
	}

	// S2 §3.2：workflow 审批单 → Temporal Signal（approve），无需 skill 查询
	if a.Kind == "workflow_signal" {
		return s.runWorkflowSignal(ctx, a, &p, "approve")
	}

	// S9.8 §7.10：只按项目识别——目标是已注册项目（非技能）→ 不做灰度（灰度是技能概念），
	// 直接 released → evolve-driver mergeToMain 快进本地 main（EVOLVE_MERGE_MODE=local，默认）。
	//
	// 归一 + 别名 + 取主目标：审批 payload 的 skill_name 存的是**多值目标串**
	//（如 `actives,tee-node,dapp,core-ui,core`，S9.8 目标可多选）或部署单元名（api/go-worker），
	// 直接拿去查 projects 永远不中 → 掉到技能分支 → skill_not_found → cd_failed，
	// 任务永久卡 approving、看板卡停在 Review（2026-10-17 卡 #97 实测）。
	projectName := project.PrimaryTargetName(p.SkillName)
	if projectName != "" {
		var projN int64
		if s.db.WithContext(ctx).Table("projects").Where("name = ?", projectName).Count(&projN).Error == nil && projN > 0 {
			if res := s.db.WithContext(ctx).Model(&db.EvolveTask{}).
				Where("approval_id = ? AND status = ?", a.ID, "approving").
				Update("status", "released"); res.Error != nil {
				log.Printf("WARN: set evolve task released for project target %s: %v", a.ApprovalNo, res.Error)
			}
			s.Log(ctx, "cd", "project_target_released", a.ApprovalNo, map[string]any{
				"target": projectName, "raw_target": p.SkillName,
				"note": "项目目标，无灰度；released → mergeToMain 快进本地 main",
			})
			return nil
		}
	}

	var skill db.Skill
	err := s.db.WithContext(ctx).Where("name = ?", p.SkillName).First(&skill).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.Log(ctx, "cd", "skill_not_found", a.ApprovalNo, ginErr(a, err))
		return fmt.Errorf("skill %s not found", p.SkillName)
	}
	if err != nil {
		return fmt.Errorf("query skill: %w", err)
	}

	switch a.Kind {
	case "skill_release":
		return s.startGrayRelease(ctx, a, &skill, p.Version, p.Image)
	case "rollback":
		return s.runRollback(ctx, a, &skill, p)
	case "takeover":
		s.Log(ctx, "cd", "takeover_logged", a.ApprovalNo,
			map[string]any{"note": "takeover 执行由 S3 干预舱实现，此处仅留痕"})
		return nil
	default:
		return fmt.Errorf("unknown approval kind: %s", a.Kind)
	}
}

// startGrayRelease 启动灰度：新版本 gray/5%，旧 live 95%（§3.4 状态 1）。
// image 非空（CI 后从 worktree 构建）→ 版本用该镜像并启版本实例（灰度真跑新代码）；
// 空 → 继承 live 镜像/实例（旧行为，无构建能力时的降级）。
// 版本行不存在时（evolve 改动未先 register）从 live 版本继承 image/config 快照补建。
func (s *Store) startGrayRelease(ctx context.Context, a *db.Approval, skill *db.Skill, version, image string) error {
	if version == "" {
		return fmt.Errorf("approval payload missing version")
	}
	now := time.Now()

	var v db.SkillVersion
	err := s.db.WithContext(ctx).Where("skill_id = ? AND version = ?", skill.ID, version).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var live db.SkillVersion
		liveErr := s.db.WithContext(ctx).
			Where("skill_id = ? AND status = ? AND traffic_split > 0", skill.ID, "live").
			First(&live).Error
		if liveErr != nil {
			return fmt.Errorf("version %s not registered and no live version to inherit from: %w", version, liveErr)
		}
		inheritImg := live.Image
		if image != "" {
			inheritImg = image // 用 CI 后构建的目标镜像（灰度跑真新代码）
		}
		v = db.SkillVersion{
			SkillID:   skill.ID,
			Version:   version,
			Image:     inheritImg,
			Config:    live.Config,
			CreatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&v).Error; err != nil {
			return fmt.Errorf("create skill version row: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("query skill version: %w", err)
	} else if image != "" {
		v.Image = image // 已有版本行（重跑）：仍以本次构建的镜像为准
	}

	// 旧 live 版本（非本版本）→ 95%
	var old db.SkillVersion
	err = s.db.WithContext(ctx).
		Where("skill_id = ? AND status = ? AND traffic_split > 0 AND version <> ?", skill.ID, "live", version).
		First(&old).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 无旧版本：直接 100% 灰度起步（首版本上线）
		old = db.SkillVersion{}
	} else if err != nil {
		return fmt.Errorf("query old live version: %w", err)
	}

	v.Status = "gray"
	v.TrafficSplit = 5
	v.GrayStage = intPtr(5)
	v.GrayStageStartedAt = &now
	v.ApprovalID = &a.ID
	if err := s.db.WithContext(ctx).Save(&v).Error; err != nil {
		return fmt.Errorf("update gray version: %w", err)
	}
	if old.ID != 0 {
		old.TrafficSplit = 95
		if err := s.db.WithContext(ctx).Save(&old).Error; err != nil {
			return fmt.Errorf("update old version traffic: %w", err)
		}
	}

	// 启版本实例（灰度真跑新代码）：版本行已就位（image/config），Up 会起容器并把 config.addr 指向它。
	// versionMgr 为 nil（无 docker socket）→ 回退旧行为（继承实例，人工部署）。
	if s.versionMgr != nil {
		if _, err := s.versionMgr.Up(ctx, skill.Name, version, "gray"); err != nil {
			s.Log(ctx, "cd", "gray_version_up_failed", a.ApprovalNo, ginErr(a, err))
			return fmt.Errorf("启动灰度版本实例: %w", err)
		}
	}

	s.Log(ctx, "cd", "gray_started", a.ApprovalNo, map[string]any{
		"skill_name": skill.Name, "version": version, "old_version": old.Version,
		"new_traffic": 5, "old_traffic": old.TrafficSplit,
	})
	return nil
}

// runRollback 把 live 版本切回 rollback_to 指向的版本（§3.2）。
func (s *Store) runRollback(ctx context.Context, a *db.Approval, skill *db.Skill, p cdPayload) error {
	target := p.RollbackTo
	if target == "" {
		return fmt.Errorf("rollback payload missing rollback_to")
	}

	var t db.SkillVersion
	err := s.db.WithContext(ctx).Where("skill_id = ? AND version = ?", skill.ID, target).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("rollback target version %s not found", target)
	}
	if err != nil {
		return fmt.Errorf("query rollback target: %w", err)
	}

	// 当前 live（非目标版本）→ draft/0
	var cur db.SkillVersion
	err = s.db.WithContext(ctx).
		Where("skill_id = ? AND status = ? AND traffic_split > 0 AND version <> ?", skill.ID, "live", target).
		First(&cur).Error
	if err == nil {
		cur.Status = "draft"
		cur.TrafficSplit = 0
		cur.GrayStage = nil
		cur.GrayStageStartedAt = nil
		if err := s.db.WithContext(ctx).Save(&cur).Error; err != nil {
			return fmt.Errorf("demote current version: %w", err)
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("query current live: %w", err)
	}

	t.Status = "live"
	t.TrafficSplit = 100
	if err := s.db.WithContext(ctx).Save(&t).Error; err != nil {
		return fmt.Errorf("promote rollback target: %w", err)
	}

	s.Log(ctx, "cd", "rollback_executed", a.ApprovalNo, map[string]any{
		"skill_name": skill.Name, "from": cur.Version, "to": target,
	})
	return nil
}

// runWorkflowSignal 向 workflow 发 Temporal Signal（S2 §3.2）。
// decision：approve / reject；payload 带审批人 / 理由 / 单号（workflow 侧 approvalSignal）。
func (s *Store) runWorkflowSignal(ctx context.Context, a *db.Approval, p *cdPayload, decision string) error {
	if p.WorkflowID == "" {
		return fmt.Errorf("workflow_signal approval missing workflow_id")
	}
	if s.signalSender == nil {
		return fmt.Errorf("signal sender not configured")
	}
	payload := map[string]any{
		"approver": a.Approver, "reason": a.Reason, "approval_no": a.ApprovalNo,
	}
	if err := s.signalSender.SendSignal(ctx, p.WorkflowID, decision, payload); err != nil {
		s.Log(ctx, "cd", "workflow_signal_failed", a.ApprovalNo, ginErr(a, err))
		return fmt.Errorf("send %s signal to %s: %w", decision, p.WorkflowID, err)
	}
	s.Log(ctx, "cd", "workflow_signal_sent", a.ApprovalNo, map[string]any{
		"workflow_id": p.WorkflowID, "run_id": p.RunID, "decision": decision, "approver": a.Approver,
	})
	return nil
}

// ginErr 审计 detail 的通用错误上下文。
func ginErr(a *db.Approval, err error) map[string]any {
	return map[string]any{"approval_no": a.ApprovalNo, "kind": a.Kind, "error": err.Error()}
}
