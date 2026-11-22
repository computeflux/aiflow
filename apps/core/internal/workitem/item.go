// Package workitem 是 S9 统一工作项流水线（docs/plan/S9-统一工作项流水线.md）：
// 需求 / Bug / 自进化提案统一成一种货币（Work Item），进同一条带轻重缓急（score + P0-P3）
// 的队列，由 work-dispatch 调度器按 score 取最高分自动领取并驱动 S1 进化链，人只在审批处放行。
//
// 本包提供：工作项状态机（item.go）、轻重缓急打分模型（score.go）、work_items 表访问层（store.go）、
// 错误日志 AI 分诊（triage.go）、优先级调度（dispatch.go）、S1 结果回流（feedback.go）。
package workitem

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/project"
)

// 工作项状态（§4.2 状态机）。
const (
	StatusOpen       = "open"
	StatusTriaged    = "triaged"
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusInReview   = "in_review"
	StatusApproved   = "approved"
	StatusShipped    = "shipped"
	StatusRejected   = "rejected"
	StatusExpired    = "expired"
)

// ---- 工作项类型 / 来源 / 目标 ----

// Kind* 是工作项类型。
const (
	KindRequirement = "requirement" // 需求
	KindBug         = "bug"         // 缺陷
)

// Source* 是工作项来源。
const (
	SourceProduct  = "product"   // 产品情报拆解
	SourceErrorLog = "error_log" // 错误日志 AI 分诊
	SourceAudit    = "audit"     // 审计巡检发现
	SourceManual   = "manual"    // 人工提卡
)

// ---- 目标（S9.8 §7.10：只按项目识别，target_name 多选）----

// parseTargets 把多值 target_name（逗号分隔，如 "pycraper,core"）拆分为目标名列表：
// trim/去空/去重/保序（委托 internal/project，别名与拆分口径单一来源）。
func parseTargets(s string) []string {
	return project.ParseTargets(s)
}

// PrimaryTarget 返回多值 target_name 的主目标（第一个）；空/无值返回 ""。
// S1 单项目执行（单 worktree / 单 target 沙箱）只跑主目标（S9.8 §7.10）。
func PrimaryTarget(targetName string) string {
	names := parseTargets(targetName)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// IsTerminal 判断是否为**流水线终止态**（shipped / rejected）：不再自动推进。
// 注：rejected 仍可被看板重新 hand-off（建**新**工作项重跑），旧项留终结态历史。
func IsTerminal(status string) bool {
	switch status {
	case StatusShipped, StatusRejected:
		return true
	}
	return false
}

// IsOpen 判断是否仍处于"未完结"（可被分诊去重、可被调度领取）：
// triaged / queued / in_progress / in_review / approved。
func IsOpen(status string) bool {
	switch status {
	case StatusTriaged, StatusQueued, StatusInProgress, StatusInReview, StatusApproved:
		return true
	}
	return false
}

// validTransitions 是状态机的合法转移表（§4.2）；非法转移由 Store.UpdateStatus 拒绝。
// S9.8 §7.10 起去掉 manual_pr：所有目标（含 core 系）统一走自动进化链，人工在审批处放行、
// 合并 PR（红线不变）；失败/回滚 → rejected（终止态，看板可重新 hand-off 重跑）。
var validTransitions = map[string][]string{
	StatusOpen:       {StatusTriaged, StatusRejected},
	StatusTriaged:    {StatusQueued, StatusRejected},
	StatusQueued:     {StatusInProgress, StatusExpired, StatusRejected, StatusOpen}, // +open：看板拖回 Backlog/Todo = 收回队列重开（看板为核心）
	StatusInProgress: {StatusInReview, StatusRejected, StatusExpired},               // +expired：S1 进化单审批过期（系统驱动回流）
	StatusInReview:   {StatusApproved, StatusRejected, StatusExpired},               // +expired：审批单 24h 过期 → task expired → 回流
	StatusApproved:   {StatusShipped, StatusRejected, StatusExpired},                // +expired：灰度前审批单过期（极端竞态）
	StatusExpired:    {StatusQueued},                                                // SLA 过期后可重新入队
	// rejected 原则上无出边（终止态，看板重跑是建新工作项）。
	// 2026-11-03 例外：看板「完成执行」允许把**已执行过、有 diff** 的被驳回任务拉回 in_review，
	// 让人直接看改动决定验收/驳回，而不必重新跑一遍。
	StatusRejected: {StatusInReview},
}

// IsValidTransition 校验状态转移是否合法；同态视为合法（幂等）。
// 合法转移完全由 validTransitions 表定义：shipped/rejected 无出边 = 不可再转移。
func IsValidTransition(from, to string) bool {
	if from == to {
		return true
	}
	for _, t := range validTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// NewNo 生成工作项号：wi-<unix>-<4hex>（唯一性兜底，撞了 1062 由上层报错）。
func NewNo() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("wi-%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}
