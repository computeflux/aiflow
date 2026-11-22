package main

import (
	"strings"
	"time"
)

// executionPayload 是一条卡片执行记录（一次尝试：一个工作项 + 其 S1 链路阶段事件）。
type executionPayload struct {
	WorkItemNo     string           `json:"work_item_no"`
	Kind           string           `json:"kind"`
	Status         string           `json:"status"`
	Title          string           `json:"title"`
	TargetName     string           `json:"target_name"`
	CreatedAt      time.Time        `json:"created_at"`
	PickedAt       *time.Time       `json:"picked_at"`
	ShippedAt      *time.Time       `json:"shipped_at"`
	EvolveTaskNo   string           `json:"evolve_task_no"`
	TaskStatus     string           `json:"task_status"`
	TaskUpdatedAt  *time.Time       `json:"task_updated_at"`
	TaskReview     string           `json:"task_review"`
	TaskDiff       string           `json:"task_diff"`        // 该轮完整 diff（人工 PR 用；driver 不 push）
	Branch         string           `json:"branch"`           // evolve/<task_no>
	PushCmd        string           `json:"push_cmd"`         // git push origin evolve/<task_no>
	CompareURL     string           `json:"compare_url"`      // GitHub compare（推完分支后一键建 PR）
	MergeCommit    string           `json:"merge_commit"`     // 本地合并（local 模式）：快进到 main 的提交 sha
	TaskMergeState string           `json:"task_merge_state"` // 合入阻塞态："" = 正常；paused_base_dirty = main 有未提交改动 → 暂停合入（看板显示阻塞）
	PRUrl          string           `json:"pr_url"`           // 远端 PR 链接（pr/both 模式）
	PRError        string           `json:"pr_error"`         // 建 PR / 本地合并失败原因（非空 = 不再重试，需人工）
	CoderSession   string           `json:"coder_session"`    // coder agent 会话 ID（pi-web ?session= 查看修改过程）
	Events         []executionEvent `json:"events"`
}

// executionEvent 是执行记录中的一个阶段事件（audit_logs 行）。
type executionEvent struct {
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail"`
	Note   string    `json:"note"`
}

// normalizeJSONText 把 MySQL JSON 列读出的空值（字面量 null / 空对象 / 空数组）归为空串。
// 否则没写过 review 的任务会把 "null" 当成一条有效记录发给前端，看板多出无意义的入口。
func normalizeJSONText(s string) string {
	switch strings.TrimSpace(s) {
	case "", "null", "{}", "[]":
		return ""
	}
	return s
}
