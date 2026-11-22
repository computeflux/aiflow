package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// 灰度自动化（docs/plan/S1-进化闭环.md §3.4）：api 进程内 1 分钟 ticker 驱动。
//
// 状态全部持久化在 skill_versions（status/traffic_split/gray_stage/
// gray_stage_started_at），api 重启后自动续推。
//
// 档位：5 → 50 → 100，每级一个窗口（GRAY_STAGE_WINDOW_SEC 秒或
// GRAY_STAGE_MIN_RUNS 次运行先到）。窗口内该版本失败率 > GRAY_FAIL_RATE
// 或 p99 延迟 > limits.timeout_sec × 0.8 → 恶化 → 一键回滚。
// 100% 窗口健康后：旧版本 retired/0，新版本 live，evolve_tasks→released。

const (
	grayStage5   = 5
	grayStage50  = 50
	grayStage100 = 100
)

// TickGray 扫所有 status='gray' 的版本，推进或回滚。返回处理（变更）的版本数。
func (s *Store) TickGray(ctx context.Context) (int, error) {
	var versions []db.SkillVersion
	if err := s.db.WithContext(ctx).
		Where("status = ? AND traffic_split > 0", "gray").
		Find(&versions).Error; err != nil {
		return 0, fmt.Errorf("list gray versions: %w", err)
	}

	changed := 0
	for i := range versions {
		v := &versions[i]
		if err := s.tickGrayOne(ctx, v); err != nil {
			// 单个版本失败不阻塞其它版本；状态已持久化，下轮重试
			log.Printf("WARN: gray tick skill=%s v%s: %v", v.Version, v.Version, err)
			continue
		}
		changed++
	}
	return changed, nil
}

func (s *Store) tickGrayOne(ctx context.Context, v *db.SkillVersion) error {
	var skill db.Skill
	if err := s.db.WithContext(ctx).Where("id = ?", v.SkillID).First(&skill).Error; err != nil {
		return fmt.Errorf("query skill: %w", err)
	}

	if v.GrayStageStartedAt == nil {
		now := time.Now()
		v.GrayStageStartedAt = &now
		return s.db.WithContext(ctx).Save(v).Error
	}
	if v.GrayStage == nil || *v.GrayStage == 0 {
		stage := grayStage5
		v.GrayStage = &stage
	}

	// 窗口是否到期（时长或运行次数先到）
	runs, err := s.versionWindowRuns(ctx, &skill, v)
	if err != nil {
		return err
	}
	windowDone := time.Since(*v.GrayStageStartedAt) >= time.Duration(s.cfg.GrayStageWindowSec)*time.Second ||
		len(runs) >= s.cfg.GrayStageMinRuns
	// 100% 档完成需要真实流量证据：窗口内 0 次运行不晋级（防低流量技能“零证据”直接 live）；
	// 5/50 档保持“时长或次数先到”（流量在逐步引入，时长到期即该放量）。
	if *v.GrayStage == grayStage100 {
		windowDone = len(runs) >= s.cfg.GrayStageMinRuns
	}
	if !windowDone {
		return nil
	}

	// 窗口内健康评估
	failRate := failRate(runs)
	p99, hasP99 := p99Latency(runs)
	timeoutMs := skillTimeoutMs(skill.Limits)
	degraded := false
	if total := len(runs); total > 0 && failRate > s.cfg.GrayFailRate {
		degraded = true
	}
	if hasP99 && timeoutMs > 0 && float64(p99) > float64(timeoutMs)*0.8 {
		degraded = true
	}

	if degraded {
		return s.rollbackGray(ctx, &skill, v)
	}
	return s.advanceGray(ctx, &skill, v)
}

// versionWindowRuns 取窗口内（gray_stage_started_at 起）该版本的执行记录。
func (s *Store) versionWindowRuns(ctx context.Context, skill *db.Skill, v *db.SkillVersion) ([]db.SkillRun, error) {
	var runs []db.SkillRun
	err := s.db.WithContext(ctx).
		Where("skill_name = ? AND version = ? AND created_at >= ?", skill.Name, v.Version, *v.GrayStageStartedAt).
		Order("created_at").
		Find(&runs).Error
	return runs, err
}

// advanceGray 健康 → 推进档位（5→50→100；100 窗口结束 → 完成）。
func (s *Store) advanceGray(ctx context.Context, skill *db.Skill, v *db.SkillVersion) error {
	now := time.Now()
	old := s.findOtherLiveVersion(ctx, skill, v.Version)

	switch *v.GrayStage {
	case grayStage5:
		v.TrafficSplit = 50
		v.GrayStage = intPtr(grayStage50)
		if old != nil {
			old.TrafficSplit = 50
			s.db.WithContext(ctx).Save(old)
		}
	case grayStage50:
		v.TrafficSplit = 100
		v.GrayStage = intPtr(grayStage100)
		if old != nil {
			old.TrafficSplit = 0
			s.db.WithContext(ctx).Save(old)
		}
	default: // 100 窗口健康 → 完成
		v.Status = "live"
		if old == nil {
			// 100% 档时旧版本流量已清零，findOtherLiveVersion（仅 traffic>0）查不到；
			// 不带流量条件再查一次，确保旧版本被置 retired（§3.4：旧版本 retired/0）。
			var o2 db.SkillVersion
			if err := s.db.WithContext(ctx).
				Where("skill_id = ? AND status = ? AND version <> ?", skill.ID, "live", v.Version).
				First(&o2).Error; err == nil {
				old = &o2
			}
		}
		if old != nil {
			old.Status = "retired"
			old.TrafficSplit = 0
			s.db.WithContext(ctx).Save(old)
		}
		// 进化单收尾：released
		if v.ApprovalID != nil {
			res := s.db.WithContext(ctx).Model(&db.EvolveTask{}).
				Where("approval_id = ? AND status <> ?", *v.ApprovalID, "released").
				Update("status", "released")
			if res.Error != nil {
				return res.Error
			}
		}
	}
	v.GrayStageStartedAt = &now
	if err := s.db.WithContext(ctx).Save(v).Error; err != nil {
		return err
	}

	action := "gray_advanced"
	if *v.GrayStage == grayStage100 && v.Status == "live" {
		action = "gray_completed"
	}
	s.Log(ctx, "gray", action, fmt.Sprintf("%s v%s", skill.Name, v.Version), map[string]any{
		"stage": *v.GrayStage, "traffic": v.TrafficSplit, "old_version": oldVersion(old),
	})
	if action == "gray_completed" {
		s.notify.SendAlert(ctx, notify.Alert{
			Title: fmt.Sprintf("灰度完成 %s v%s", skill.Name, v.Version),
			Body:  "新版本 100% 上线，旧版本已 retired。",
			Level: "info",
		})
	}
	return nil
}

// rollbackGray 恶化 → 一键回滚（§3.4 状态 4）：
// 新版本 0/retired，旧版本恢复 100%，evolve_tasks→rolled_back，
// 写 interventions（manual：S5 前版本实例容器需人工停）+ audit_logs + 告警。
func (s *Store) rollbackGray(ctx context.Context, skill *db.Skill, v *db.SkillVersion) error {
	old := s.findOtherLiveVersion(ctx, skill, v.Version)

	v.TrafficSplit = 0
	v.Status = "retired"
	v.GrayStage = nil
	v.GrayStageStartedAt = nil
	if err := s.db.WithContext(ctx).Save(v).Error; err != nil {
		return err
	}
	if old != nil {
		old.TrafficSplit = 100
		old.Status = "live"
		s.db.WithContext(ctx).Save(old)
	}

	if v.ApprovalID != nil {
		res := s.db.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("approval_id = ?", *v.ApprovalID).
			Update("status", "rolled_back")
		if res.Error != nil {
			return res.Error
		}
	}

	// S5（deployctl version down）：回滚 = 分流清零 + 新实例停（自动；无版本管理时人工）
	stopped := false
	if s.versionMgr != nil {
		if err := s.versionMgr.StopVersion(ctx, skill.Name, v.Version); err != nil {
			log.Printf("WARN: version down %s v%s: %v（需人工停版本实例容器）", skill.Name, v.Version, err)
		} else {
			stopped = true
		}
	}
	var note string
	if stopped {
		note = fmt.Sprintf("灰度恶化自动回滚：%s v%s 流量清零（失败率/p99 超阈值），已切回 v%s，版本实例容器已停（deployctl version down）。",
			skill.Name, v.Version, oldVersion(old))
	} else {
		note = fmt.Sprintf("灰度恶化自动回滚：%s v%s 流量清零（失败率/p99 超阈值），已切回 v%s。"+
			"请人工停版本实例容器（deployctl version down %s %s）。",
			skill.Name, v.Version, oldVersion(old), skill.Name, v.Version)
	}
	if err := s.CreateIntervention(ctx, skill.Name, "manual", note); err != nil {
		return err
	}

	// S3 §3.1 灰度联动：窗口内失败率恶化 → 同时 open 该 skill 熔断（回滚与熔断共用同一 interventions 记录）
	if s.breaker != nil {
		s.breaker.Get(skill.Name).ForceOpen(ctx)
	}

	s.Log(ctx, "gray", "gray_rollback", fmt.Sprintf("%s v%s", skill.Name, v.Version), map[string]any{
		"rolled_back_to": oldVersion(old), "intervention": note, "circuit": "open",
	})
	s.notify.SendAlert(ctx, notify.Alert{
		Title: fmt.Sprintf("灰度恶化已回滚 %s v%s", skill.Name, v.Version),
		Body:  note,
		Level: "critical",
	})
	return nil
}

// findOtherLiveVersion 找同一 skill 的其它 live 版本（旧版本）。
func (s *Store) findOtherLiveVersion(ctx context.Context, skill *db.Skill, excludeVersion string) *db.SkillVersion {
	var old db.SkillVersion
	if err := s.db.WithContext(ctx).
		Where("skill_id = ? AND status = ? AND traffic_split > 0 AND version <> ?", skill.ID, "live", excludeVersion).
		First(&old).Error; err != nil {
		return nil
	}
	return &old
}

// failRate 窗口内失败率（fail/timeout 计失败）。
func failRate(runs []db.SkillRun) float64 {
	if len(runs) == 0 {
		return 0
	}
	fails := 0
	for _, r := range runs {
		// skill_runs.status 的真实取值见 skillgateway.RunStatus*：ok / fail / timeout / rejected。
		// 2026-09-30 修：之前只数 fail/timeout，漏了 rejected（限流/输入被拒也是失败）——
		// 会让“全被拒”的灰度版本被判健康并推到 100%。非 ok 一律计失败。
		if r.Status != "ok" {
			fails++
		}
	}
	return float64(fails) / float64(len(runs))
}

// p99Latency 窗口内 p99 延迟（ms）；无数据返回 false。
func p99Latency(runs []db.SkillRun) (int64, bool) {
	var lats []int64
	for _, r := range runs {
		if r.LatencyMs != nil {
			lats = append(lats, *r.LatencyMs)
		}
	}
	if len(lats) == 0 {
		return 0, false
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	idx := int(math.Ceil(0.99*float64(len(lats)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(lats) {
		idx = len(lats) - 1
	}
	return lats[idx], true
}

// skillTimeoutMs 解析 skills.limits JSON 的 timeout_sec（秒→毫秒）；缺省 0（不判 p99）。
func skillTimeoutMs(limitsJSON string) int64 {
	if limitsJSON == "" {
		return 0
	}
	var limits struct {
		TimeoutSec int64 `json:"timeout_sec"`
	}
	if err := json.Unmarshal([]byte(limitsJSON), &limits); err != nil {
		return 0
	}
	return limits.TimeoutSec * 1000
}

func intPtr(v int) *int { return &v }

func oldVersion(v *db.SkillVersion) string {
	if v == nil {
		return ""
	}
	return v.Version
}
