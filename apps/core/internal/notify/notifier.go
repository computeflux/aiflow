// Package notify 是统一告警 / 审批通知接口（docs/plan/S1-进化闭环.md §3.2）。
//
// S1 提供接口 + no-op 默认实现；S4 接 Matrix（MATRIX_ENABLED=true 时启用，
// 见 docs/plan/evolve-plan.md §3.3 的 MATRIX_* 键）。所有调用方只依赖 Notifier
// 接口，替换实现零改动。
package notify

import (
	"context"
	"log"
	"os"
	"time"
)

// Alert 是运维 / 系统告警（audit 巡检、灰度回滚、越界打回等）。
type Alert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Level string `json:"level"` // info | warning | critical
}

// ApprovalNotice 是审批单通知（新建 / 决策 / 过期升级）。
// Status 为空 = 新建（进 #approvals，approval.request 协议，transform-plan §10.2）；
// Status = approved|rejected = 决策通知；Expired = true 走 #ops-alerts 升级。
type ApprovalNotice struct {
	ID         uint      `json:"id"`
	ApprovalNo string    `json:"approval_no"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject"`
	Proposer   string    `json:"proposer"`
	ExpiresAt  time.Time `json:"expires_at"`
	Expired    bool      `json:"expired"`            // true = 超时无人处理（升级通知）
	Status     string    `json:"status,omitempty"`   // approved | rejected
	Approver   string    `json:"approver,omitempty"` // 决策人（board 用户 / matrix 用户）
	Summary    string    `json:"summary,omitempty"`  // diff 摘要 / 报告路径（approval.request 的 diff 字段）
}

// Notifier 是通知接口：告警 + 审批通知 + Agent 日志。
type Notifier interface {
	SendAlert(ctx context.Context, a Alert) error
	SendApproval(ctx context.Context, n ApprovalNotice) error
	SendAgentLog(ctx context.Context, msg string) error
}

// Noop 是默认实现：不发送（本地开发 / Matrix 未部署）。
type Noop struct{}

func (Noop) SendAlert(context.Context, Alert) error             { return nil }
func (Noop) SendApproval(context.Context, ApprovalNotice) error { return nil }
func (Noop) SendAgentLog(context.Context, string) error         { return nil }

// New 按环境变量返回 Notifier。
// MATRIX_ENABLED=true 且 MATRIX_BOT_TOKEN 非空 → Matrix 实现（S4）；否则 no-op。
func New() Notifier {
	if os.Getenv("MATRIX_ENABLED") == "true" {
		if m := NewMatrix(); m != nil {
			log.Printf("notify: Matrix 通知器已启用（%s）", os.Getenv("MATRIX_HOMESERVER"))
			return m
		}
		log.Printf("notify: MATRIX_ENABLED=true 但 MATRIX_BOT_TOKEN 未设置，暂用 no-op")
	}
	return Noop{}
}
