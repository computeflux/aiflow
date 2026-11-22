// ocr_reviews.go：OCR 审核数据查询 API（wi-1793175196-bd90）。
//
// 为 OCR review-web 提供数据支撑：
//
//	GET /ocr/reviews         审核记录列表（分页 / 筛选）
//	GET /ocr/reviews/:task_no 单条审核过程详情（各步骤状态 / 时间戳 / 产物）
//
// 数据来源（不新增表，复用现有存储）：
//   - evolve_tasks 表：review 列（JSON，reviewer 原始输出 {"ok":true,"comments":[...]}）、
//     status、diff、created_at / updated_at
//   - audit_logs 表：target = task_no 的事件流
//     （coder_done / reviewer_done / reviewer_retry / sandbox_done / approval_created 等）
//
// 只读接口，需登录（requireAuth）；不写任何数据。
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
)

// ---- 响应结构 ----

// ocrReviewComment 单条审核评论（与 agentcli.Comment 同构）。
type ocrReviewComment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ocrReviewSummary 评论按严重度聚合计数。
type ocrReviewSummary struct {
	Total   int `json:"total"`
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

// ocrReviewListItem 审核记录列表项。
type ocrReviewListItem struct {
	TaskNo       string    `json:"task_no"`
	TargetName   string    `json:"target_name"`
	Status       string    `json:"status"`
	Source       string    `json:"source"`
	CommentCount int       `json:"comment_count"`
	ErrorCount   int       `json:"error_count"`
	WarningCount int       `json:"warning_count"`
	InfoCount    int       `json:"info_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ocrReviewStep 审核过程中的一个步骤。
// Status：pending（未开始）/ running（进行中）/ done（已完成）。
type ocrReviewStep struct {
	Name   string     `json:"name"`
	Status string     `json:"status"`
	At     *time.Time `json:"at"`
	Detail any        `json:"detail,omitempty"`
}

// ocrReviewEvent 审核过程中的一个审计事件（audit_logs 行）。
type ocrReviewEvent struct {
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail"`
	Note   string    `json:"note"`
}

// ocrReviewParsed 解析后的审核输出（evolve_tasks.review 列）。
type ocrReviewParsed struct {
	Agent           string             `json:"agent"`
	Comments        []ocrReviewComment `json:"comments"`
	Summary         ocrReviewSummary   `json:"summary"`
	ReviewerRetries int                `json:"reviewer_retries"` // reviewer 执行失败重试次数（audit_logs reviewer_retry 计数）
}

// ocrReviewArtifacts 审核产物（原始数据，供 review-web 展示 / 下载）。
type ocrReviewArtifacts struct {
	Diff       string `json:"diff"`
	ReviewJSON string `json:"review_json"`
}

// ocrReviewDetail 单条审核过程详情。
type ocrReviewDetail struct {
	Task      db.EvolveTask      `json:"task"`
	Review    *ocrReviewParsed   `json:"review"`
	Steps     []ocrReviewStep    `json:"steps"`
	Events    []ocrReviewEvent   `json:"events"`
	Artifacts ocrReviewArtifacts `json:"artifacts"`
}

// ---- 内部工具 ----

// parseReviewJSON 解析 evolve_tasks.review 列（JSON 字符串）为结构化评论。
// 返回 nil 表示无有效审核数据（空 / JSON null / 解析失败 / ok=false）。
func parseReviewJSON(raw string) *ocrReviewParsed {
	if raw == "" || raw == "null" {
		return nil
	}
	var data struct {
		OK       bool               `json:"ok"`
		Comments []ocrReviewComment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil || !data.OK {
		return nil
	}
	if data.Comments == nil {
		data.Comments = []ocrReviewComment{}
	}
	summary := ocrReviewSummary{Total: len(data.Comments)}
	for _, c := range data.Comments {
		switch c.Level {
		case "error":
			summary.Error++
		case "warning":
			summary.Warning++
		default:
			summary.Info++
		}
	}
	return &ocrReviewParsed{Comments: data.Comments, Summary: summary}
}

// reviewStatusOrder 进化单状态机顺序（用于推导步骤完成度）。
// open → coding → reviewing → sandboxing → testing → approving → released
var reviewStatusOrder = map[string]int{
	"open": 0, "coding": 1, "reviewing": 2, "sandboxing": 3,
	"testing": 4, "approving": 5, "released": 6,
}

// buildReviewSteps 从任务状态 + 审计事件推导各步骤的状态与时间戳。
// 对 cancelled / rolled_back 任务，从事件流推导最远到达的步骤。
func buildReviewSteps(task *db.EvolveTask, events []ocrReviewEvent) []ocrReviewStep {
	currentOrder, known := reviewStatusOrder[task.Status]
	if !known {
		// cancelled / rolled_back：从事件推导最远到达的步骤
		currentOrder = 0
		for _, e := range events {
			switch e.Action {
			case "coder_done":
				if currentOrder < 1 {
					currentOrder = 1
				}
			case "reviewer_done":
				if currentOrder < 2 {
					currentOrder = 2
				}
			case "sandbox_done":
				if currentOrder < 3 {
					currentOrder = 3
				}
			case "approval_created":
				if currentOrder < 5 {
					currentOrder = 5
				}
			}
		}
	}

	// 事件索引（取每个 action 的首次出现）
	eventIdx := make(map[string]ocrReviewEvent, len(events))
	for _, e := range events {
		if _, ok := eventIdx[e.Action]; !ok {
			eventIdx[e.Action] = e
		}
	}

	type stepDef struct {
		name     string
		doneAt   int    // 状态机顺序值：currentOrder > doneAt → done；== → running
		eventKey string // 标记该步骤完成的审计事件 action（空 = 无对应事件）
	}
	defs := []stepDef{
		{"created", 0, ""},
		{"coding", 1, "coder_done"},
		{"reviewing", 2, "reviewer_done"},
		{"sandboxing", 3, "sandbox_done"},
		{"testing", 4, ""},
		{"approving", 5, "approval_created"},
		{"released", 6, ""},
	}

	steps := make([]ocrReviewStep, 0, len(defs))
	for _, sd := range defs {
		step := ocrReviewStep{Name: sd.name}
		switch {
		case sd.name == "created":
			step.Status = "done"
			step.At = &task.CreatedAt
		case currentOrder > sd.doneAt:
			step.Status = "done"
			fillStepFromEvent(&step, eventIdx, sd.eventKey)
		case currentOrder == sd.doneAt:
			if sd.eventKey != "" {
				if ev, ok := eventIdx[sd.eventKey]; ok {
					step.Status = "done"
					step.At = &ev.At
					fillStepDetail(&step, ev.Detail)
				} else {
					step.Status = "running"
				}
			} else {
				step.Status = "running"
			}
		default:
			step.Status = "pending"
		}
		steps = append(steps, step)
	}
	return steps
}

// fillStepFromEvent 从事件索引填充步骤的时间戳与详情（步骤已完成时）。
func fillStepFromEvent(step *ocrReviewStep, idx map[string]ocrReviewEvent, key string) {
	if key == "" {
		return
	}
	if ev, ok := idx[key]; ok {
		step.At = &ev.At
		fillStepDetail(step, ev.Detail)
	}
}

// fillStepDetail 解析审计事件的 detail JSON 字段，填充到步骤 Detail。
func fillStepDetail(step *ocrReviewStep, detailJSON string) {
	if detailJSON == "" || detailJSON == "null" {
		return
	}
	var v any
	if err := json.Unmarshal([]byte(detailJSON), &v); err == nil {
		step.Detail = v
	}
}

// ---- 路由注册 ----

// registerOCRReviewRoutes 注册 OCR 审核数据查询 REST 端点（wi-1793175196-bd90）。
// 只读接口，需登录；不写任何数据。
func registerOCRReviewRoutes(r *gin.Engine, database *db.DB, requireAuth gin.HandlerFunc) {
	// GET /ocr/reviews — 审核记录列表（分页 / 筛选）
	// 查询参数：page, page_size, task_no(模糊), target_name, status, source
	r.GET("/ocr/reviews", requireAuth, func(ctx *gin.Context) {
		page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
		pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
		if page < 1 {
			page = 1
		}
		if pageSize < 1 || pageSize > 100 {
			pageSize = 20
		}

		// 只查有审核数据的进化单（review 列非空且非 JSON null）
		query := database.Model(&db.EvolveTask{}).
			Where("review IS NOT NULL AND review != 'null'")

		if v := ctx.Query("task_no"); v != "" {
			query = query.Where("task_no LIKE ?", "%"+v+"%")
		}
		if v := ctx.Query("target_name"); v != "" {
			query = query.Where("target_name = ?", v)
		}
		if v := ctx.Query("status"); v != "" {
			query = query.Where("status = ?", v)
		}
		if v := ctx.Query("source"); v != "" {
			query = query.Where("source = ?", v)
		}

		var total int64
		if err := query.Count(&total).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "ocr-reviews list", err))
			return
		}

		var tasks []db.EvolveTask
		if err := query.Order("id DESC").
			Offset((page - 1) * pageSize).Limit(pageSize).
			Find(&tasks).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "ocr-reviews list", err))
			return
		}

		items := make([]ocrReviewListItem, 0, len(tasks))
		for _, t := range tasks {
			item := ocrReviewListItem{
				TaskNo: t.TaskNo, TargetName: t.TargetName,
				Status: t.Status, Source: t.Source,
				CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			}
			if p := parseReviewJSON(t.Review); p != nil {
				item.CommentCount = p.Summary.Total
				item.ErrorCount = p.Summary.Error
				item.WarningCount = p.Summary.Warning
				item.InfoCount = p.Summary.Info
			}
			items = append(items, item)
		}

		ctx.JSON(http.StatusOK, gin.H{
			"items": items, "total": total, "page": page, "page_size": pageSize,
		})
	})

	// GET /ocr/reviews/:task_no — 单条审核过程详情
	// 返回：任务元数据 + 解析后的审核评论 + 步骤时间线 + 审计事件流 + 原始产物
	r.GET("/ocr/reviews/:task_no", requireAuth, func(ctx *gin.Context) {
		taskNo := ctx.Param("task_no")

		var task db.EvolveTask
		if err := database.Where("task_no = ?", taskNo).First(&task).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "review not found: " + taskNo})
			return
		}

		// 审计事件（target = task_no，按时间序）
		var logs []db.AuditLog
		if err := database.Where("target = ?", taskNo).Order("id ASC").Find(&logs).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "ocr-reviews detail", err))
			return
		}
		events := make([]ocrReviewEvent, 0, len(logs))
		for _, l := range logs {
			events = append(events, ocrReviewEvent{
				Action: l.Action, Actor: l.Actor,
				At: l.CreatedAt, Detail: l.Detail, Note: l.Note,
			})
		}

		// 解析审核输出 + 从审计事件提取 agent 名与重试次数
		review := parseReviewJSON(task.Review)
		if review != nil {
			for _, e := range events {
				switch e.Action {
				case "reviewer_done":
					if e.Detail != "" && e.Detail != "null" {
						var det map[string]any
						if json.Unmarshal([]byte(e.Detail), &det) == nil {
							if agent, ok := det["agent"].(string); ok {
								review.Agent = agent
							}
						}
					}
				case "reviewer_retry":
					review.ReviewerRetries++
				}
			}
		}

		ctx.JSON(http.StatusOK, gin.H{"review": ocrReviewDetail{
			Task:   task,
			Review: review,
			Steps:  buildReviewSteps(&task, events),
			Events: events,
			Artifacts: ocrReviewArtifacts{
				Diff:       task.Diff,
				ReviewJSON: task.Review,
			},
		}})
	})
}
