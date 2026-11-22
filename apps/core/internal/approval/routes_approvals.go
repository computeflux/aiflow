package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/notify"
)

// routes_approvals.go：审批单列表/详情/裁决（从 routes.go 拆出，纯机械搬迁）。

// truncateDiff payload 列表视图里 diff 截断到 4KB（详情接口给全量）。
func truncateDiff(payload string) string {
	var p struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Diff == "" {
		return payload
	}
	const limit = 4 << 10
	if len(p.Diff) <= limit {
		return payload
	}
	p.Diff = p.Diff[:limit] + fmt.Sprintf("\n... [diff truncated, %d bytes total; GET /approvals/:id for full]", len(p.Diff))
	b, _ := json.Marshal(p)
	return string(b)
}

func (s *Store) listApprovals(ctx *gin.Context) {
	status := ctx.Query("status")
	limit, _ := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows []db.Approval
	q := s.db.WithContext(ctx).Order("id DESC").Limit(limit)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Find(&rows).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, a := range rows {
		out = append(out, approvalView(a, true))
	}
	ctx.JSON(http.StatusOK, gin.H{"approvals": out})
}

func (s *Store) getApproval(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var a db.Approval
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&a).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "approval not found"})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"approval": approvalView(a, false)})
}

func approvalView(a db.Approval, truncate bool) gin.H {
	payload := a.Payload
	if truncate {
		payload = truncateDiff(payload)
	}
	return gin.H{
		"id": a.ID, "approval_no": a.ApprovalNo, "kind": a.Kind, "subject": a.Subject,
		"payload": json.RawMessage(payload), "status": a.Status, "proposer": a.Proposer,
		"approver": a.Approver, "reason": a.Reason, "expires_at": a.ExpiresAt,
		"created_at": a.CreatedAt, "decided_at": a.DecidedAt,
	}
}

type decideBody struct {
	Approver string `json:"approver"`
	Reason   string `json:"reason"`
}

// 决策 sentinel 错误（供路由映射 HTTP 状态码）。
var (
	ErrApproverRequired = errors.New("approver required")
	ErrApprovalNotFound = errors.New("approval not found")
	ErrApprovalDecided  = errors.New("approval already decided")
)

// Decide 审批决策（approve/reject）：运维中心路由与看板直接审批（S9 看板 Review 直接审批）共用同一审批链。
// 落库 + 留痕 + 通知；approve 后执行 CD（失败 → 错误，审批已落库并留痕）；
// reject 同步取消关联进化单（approving→cancelled，与 expiry 过期路径对称）+ 发 workflow reject Signal（S2）。
func (s *Store) Decide(ctx context.Context, id int, decision, approver, reason string) (*db.Approval, error) {
	if strings.TrimSpace(approver) == "" {
		return nil, ErrApproverRequired
	}
	if decision != "approve" && decision != "reject" {
		return nil, fmt.Errorf("decision must be approve|reject")
	}
	var a db.Approval
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&a).Error; err != nil {
		return nil, ErrApprovalNotFound
	}
	if a.Status != "pending" {
		return nil, fmt.Errorf("%w: %s", ErrApprovalDecided, a.Status)
	}
	switch decision {
	case "approve":
		a.Status = "approved"
	case "reject":
		a.Status = "rejected"
	}
	a.Approver = approver
	a.Reason = reason
	now := time.Now()
	a.DecidedAt = &now
	if err := s.db.WithContext(ctx).Save(&a).Error; err != nil {
		return nil, err
	}
	if decision == "approve" {
		s.Log(ctx, approver, "approval_approved", a.ApprovalNo, gin.H{"kind": a.Kind, "subject": a.Subject, "reason": reason})
		// S4 §3.2：决策通知 → #approvals（matrix 内 approve 也走这里，留痕 + 确认消息）
		_ = s.notify.SendApproval(ctx, notify.ApprovalNotice{
			ID: a.ID, ApprovalNo: a.ApprovalNo, Kind: a.Kind, Subject: a.Subject,
			Proposer: a.Proposer, ExpiresAt: expiryTime(a), Status: "approved", Approver: approver,
		})
		// CD：approve 后执行（skill_release → 灰度流；rollback → 切回 rollback_to）
		cdCtx, cdCancel := context.WithTimeout(ctx, 60*time.Second)
		cdErr := s.RunCD(cdCtx, &a)
		cdCancel()
		if cdErr != nil {
			s.Log(ctx, "cd", "cd_failed", a.ApprovalNo, gin.H{"error": cdErr.Error()})
			return &a, fmt.Errorf("approved, but CD failed: %w", cdErr)
		}
	} else {
		s.Log(ctx, approver, "approval_rejected", a.ApprovalNo, gin.H{"kind": a.Kind, "subject": a.Subject, "reason": reason})
		// S9 §7.7：驳回 → 关联进化单同步 cancelled（防 task 卡在 approving 导致工作项卡死 / 去重阻塞）
		if res := s.db.WithContext(ctx).Model(&db.EvolveTask{}).
			Where("approval_id = ? AND status = ?", a.ID, "approving").
			Update("status", "cancelled"); res.Error != nil {
			log.Printf("WARN: cancel evolve task for approval %s: %v", a.ApprovalNo, res.Error)
		} else if res.RowsAffected > 0 {
			s.Log(ctx, approver, "evolve_task_cancelled", a.ApprovalNo, gin.H{"kind": a.Kind, "subject": a.Subject, "reason": reason})
		}
		_ = s.notify.SendApproval(ctx, notify.ApprovalNotice{
			ID: a.ID, ApprovalNo: a.ApprovalNo, Kind: a.Kind, Subject: a.Subject,
			Proposer: a.Proposer, ExpiresAt: expiryTime(a), Status: "rejected", Approver: approver,
		})
		// S2 §3.2：workflow 审批单驳回也必须发 Signal（reject），否则 workflow 会一直等到超时
		if a.Kind == "workflow_signal" {
			var p cdPayload
			if err := json.Unmarshal([]byte(a.Payload), &p); err == nil {
				if err := s.runWorkflowSignal(ctx, &a, &p, "reject"); err != nil {
					s.Log(ctx, "cd", "cd_failed", a.ApprovalNo, gin.H{"error": err.Error()})
					return &a, fmt.Errorf("rejected, but signal failed: %w", err)
				}
			} else {
				return &a, fmt.Errorf("rejected, but payload parse failed")
			}
		}
	}
	return &a, nil
}

// decideResponse 把 Decide 的 sentinel 错误映射为 HTTP 状态码（400/404/409/200）。
// 审批已落库（a != nil）时 CD 失败不返回 5xx：主链路审批操作保持可用，
// CD 失败经 cd_error 透出并已在 audit_logs 留痕（cd_failed）。
func (s *Store) decideResponse(ctx *gin.Context, a *db.Approval, err error) {
	if err == nil {
		ctx.JSON(http.StatusOK, gin.H{"ok": true, "approval": approvalView(*a, false)})
		return
	}
	switch {
	case errors.Is(err, ErrApproverRequired):
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, ErrApprovalNotFound):
		ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, ErrApprovalDecided):
		ctx.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		if a != nil {
			ctx.JSON(http.StatusOK, gin.H{"ok": true, "cd_error": err.Error(), "approval": approvalView(*a, false)})
		} else {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
	}
}

func (s *Store) approve(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var body decideBody
	_ = ctx.ShouldBindJSON(&body)
	a, err := s.Decide(ctx, id, "approve", body.Approver, body.Reason)
	s.decideResponse(ctx, a, err)
}

func (s *Store) reject(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var body decideBody
	_ = ctx.ShouldBindJSON(&body)
	a, err := s.Decide(ctx, id, "reject", body.Approver, body.Reason)
	s.decideResponse(ctx, a, err)
}

// expiryTime 取审批单过期时间（未设置返回零值）。
func expiryTime(a db.Approval) time.Time {
	if a.ExpiresAt != nil {
		return *a.ExpiresAt
	}
	return time.Time{}
}
