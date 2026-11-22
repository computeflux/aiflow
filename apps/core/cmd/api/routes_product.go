package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/authn"
	"github.com/wetee/ai-workflow/internal/product"
	"github.com/wetee/ai-workflow/internal/workflows"
	"github.com/wetee/ai-workflow/internal/workitem"
	"go.temporal.io/sdk/client"
)

// registerProductRoutes S7 产品情报（docs/plan/S7-产品情报.md）：
// 内部知识入库 + 产品 Agent（RAG 接地对话 / 五要素变更需求文档 / 会议纪要拆解 → 任务卡）。
// 红线：secret 内容不送外部 LLM；目标含 core 系 → 仅 manual_pr（不进自动进化链）。
func registerProductRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	c := deps.temporal
	productStore := deps.productStore
	productAgent := deps.productAgent

	// S7 §3.1：内部知识入库（source=internal → 进程内 清洗/分块/向量化 → Qdrant + knowledge_docs 去重基线）；
	// 其余 source 保持原外部抓取 Temporal workflow（外部链路不变）。
	r.POST("/ingest", func(ctx *gin.Context) {
		raw, err := io.ReadAll(ctx.Request.Body)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var probe struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
			return
		}
		if probe.Source == "internal" {
			var in product.IngestInput
			if err := json.Unmarshal(raw, &in); err != nil {
				ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid internal ingest input: " + err.Error()})
				return
			}
			n, err := product.IngestInternal(ctx, productStore, deps.qdrantClient, deps.llm, in)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			ctx.JSON(http.StatusOK, gin.H{"source": "internal", "chunks": n})
			return
		}
		var input workflows.FetchWorkflowInput
		if err := json.Unmarshal(raw, &input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: deps.cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("ingest-%s-%s", input.Source, uuid.NewString())}, workflows.FetchWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()})
	})

	r.POST("/product/chat", requireAuth(), func(ctx *gin.Context) {
		var body struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Query) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
			return
		}
		res, err := productAgent.Chat(ctx, authn.Roles(ctx), body.Query, body.K)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, res)
	})
	r.POST("/product/doc", requireAuth(), func(ctx *gin.Context) {
		var in product.DocInput
		if err := ctx.ShouldBindJSON(&in); err != nil || strings.TrimSpace(in.Prompt) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
			return
		}
		doc, err := productAgent.GenerateDoc(ctx, authn.Roles(ctx), in)
		if err != nil {
			ctx.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusCreated, doc)
	})
	r.POST("/product/decompose", requireAuth(), func(ctx *gin.Context) {
		var body struct {
			Text    string `json:"text"`
			BoardID uint   `json:"board_id"`
			ListID  uint   `json:"list_id"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Text) == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
			return
		}
		if body.ListID == 0 {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "list_id is required"})
			return
		}
		result, err := product.Decompose(ctx, deps.llm, body.Text)
		if err != nil {
			// 超时单独走 504：前端能区分「模型太慢」与「文档/参数有问题」，
			// 前者该调大 DECOMPOSE_TIMEOUT_SEC 或拆短文档，后者该改内容。
			if errors.Is(err, product.ErrDecomposeTimeout) {
				ctx.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
				return
			}
			ctx.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		cards, err := product.CreateCards(ctx, deps.database.DB, productStore, body.BoardID, body.ListID, authn.UserID(ctx), result, deps.workitemStore)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusCreated, gin.H{"cards": cards})
	})
	r.GET("/product/docs", requireAuth(), func(ctx *gin.Context) {
		boardID, _ := strconv.Atoi(ctx.DefaultQuery("board_id", "0"))
		if boardID <= 0 {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "board_id is required"})
			return
		}
		docs, err := productStore.ListDocs(ctx, uint(boardID), ctx.Query("status"))
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"docs": docs})
	})
	r.POST("/product/docs/:id/status", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var body struct {
			Status string `json:"status"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		switch body.Status {
		case "draft", "accepted", "rejected":
		default:
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "status must be draft|accepted|rejected"})
			return
		}
		if err := productStore.UpdateDocStatus(ctx, uint(id), body.Status); err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.PUT("/product/docs/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var body struct {
			Content string `json:"content"`
		}
		if err := ctx.ShouldBindJSON(&body); err != nil || body.Content == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
			return
		}
		if err := productStore.UpdateDocContent(ctx, uint(id), body.Content); err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// ---- S9 统一工作项（docs/plan/S9-统一工作项流水线.md §8）----
	// 需求 / Bug / 自进化统一货币 + 轻重缓急队列；调度器自动领取驱动 S1 链，人只在审批处放行。
	workitemStore := deps.workitemStore
	r.GET("/work-items", requireAuth(), func(ctx *gin.Context) {
		f := workitem.Filter{Kind: ctx.Query("kind"), Source: ctx.Query("source"), Status: ctx.Query("status")}
		if ms := ctx.Query("min_score"); ms != "" {
			if v, err := strconv.ParseFloat(ms, 64); err == nil {
				f.MinScore = v
			}
		}
		items, err := workitemStore.List(ctx, f)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
	})
	r.GET("/work-items/:no", requireAuth(), func(ctx *gin.Context) {
		item, err := workitemStore.ByNo(ctx, ctx.Param("no"))
		if err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "work item not found"})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"item": item})
	})
	// 确认分诊：open/triaged → queued（接受 AI 分诊入队被调度）；已入队/进行中 → 幂等。
	r.POST("/work-items/:no/confirm", requireAuth(), func(ctx *gin.Context) {
		no := ctx.Param("no")
		item, err := workitemStore.ByNo(ctx, no)
		if err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "work item not found"})
			return
		}
		if item.Status == workitem.StatusOpen || item.Status == workitem.StatusTriaged {
			// 状态机不允许 open → queued（跳过分诊）：未入队项先补 triaged 一步
			if err := workitem.QueueFromPreTriage(ctx, workitemStore, no, item.Status); err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "ok", "no": no})
	})
	r.POST("/work-items/:no/reject", requireAuth(), func(ctx *gin.Context) {
		var body struct {
			Reason string `json:"reason"`
		}
		_ = ctx.ShouldBindJSON(&body)
		if err := workitemStore.UpdateStatus(ctx, ctx.Param("no"), workitem.StatusRejected); err != nil {
			ctx.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "rejected", "reason": body.Reason})
	})
	lockRoute := func(locked bool) gin.HandlerFunc {
		return func(ctx *gin.Context) {
			if err := workitemStore.SetLocked(ctx, ctx.Param("no"), locked); err != nil {
				ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			ctx.JSON(http.StatusOK, gin.H{"status": "ok", "locked": locked})
		}
	}
	r.POST("/work-items/:no/lock", requireAuth(), lockRoute(true))
	r.POST("/work-items/:no/unlock", requireAuth(), lockRoute(false))
	// 手动领取：人工强制领取（触发 S1 链），用于重点攻坚 / 需人主导的场景（§7.5）。
	r.POST("/work-items/:no/pick", requireAuth(), func(ctx *gin.Context) {
		no, err := workitem.PickItem(ctx, workitemStore, ctx.Param("no"))
		if err != nil {
			ctx.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"status": "picked", "no": no})
	})
	// 手动跑一轮分诊（tools profile / 手工触发）；body 可传 {"window":"168h"} 扩大扫描窗口。
	r.POST("/work-items/triage", requireAuth(), func(ctx *gin.Context) {
		triCfg := workitem.DefaultTriageConfig()
		var body struct {
			Window string `json:"window"`
		}
		_ = ctx.ShouldBindJSON(&body)
		if body.Window != "" {
			if d, err := time.ParseDuration(body.Window); err == nil && d > 0 {
				triCfg.Window = d
			}
		}
		res, err := workitem.TriageErrorLogs(ctx, workitemStore, deps.llm, triCfg)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, res)
	})
	// 调度状态：当前 in_progress + 队列 Top N（统一队列实时视图）。
	r.GET("/work-dispatch/status", requireAuth(), func(ctx *gin.Context) {
		inProgress, _ := workitemStore.InProgressCount(ctx)
		queued, _ := workitemStore.List(ctx, workitem.Filter{Status: workitem.StatusQueued, Limit: 20})
		ctx.JSON(http.StatusOK, gin.H{"in_progress": inProgress, "queue_top": queued})
	})

	// ---- S9 §7.8 项目注册（apps/ 目录 ↔ 项目 ↔ 可执行目标）----
	registerProjectRoutes(r, deps, requireAuth)
}

// registerProjectRoutes S9 §7.8 项目注册：
// 项目列表：工作项 / 卡片的 target_name 可执行目标 = 已注册技能 + 启用项目。
func registerProjectRoutes(r *gin.Engine, deps *appDeps, requireAuth func() gin.HandlerFunc) {
	database, projectStore := deps.database, deps.projectStore
	r.GET("/projects", requireAuth(), func(ctx *gin.Context) {
		projects, err := projectStore.List(ctx)
		if err != nil {
			serverError(database, "list projects", err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"projects": projects})
	})
	// 扫描注册：扫 REPO_ROOT/apps 下目录（1 目录 = 1 项目；幂等，不覆盖用户编辑的 image/test_cmd/active）。
	r.POST("/projects/scan", requireAuth(), func(ctx *gin.Context) {
		root := os.Getenv("REPO_ROOT")
		if root == "" {
			root = "/repo"
		}
		res, err := projectStore.ScanAndRegister(ctx, root)
		if err != nil {
			serverError(database, "scan projects", err)
			return
		}
		log.Printf("projects scan: scanned=%d created=%d updated=%d", res.Scanned, res.Created, res.Updated)
		ctx.JSON(http.StatusOK, res)
	})
	// 编辑项目：image / test_cmd / description / active。
	r.PUT("/projects/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var input struct {
			Image       *string `json:"image"`
			TestCmd     *string `json:"test_cmd"`
			Description *string `json:"description"`
			Active      *bool   `json:"active"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		fields := map[string]any{}
		if input.Image != nil {
			fields["image"] = *input.Image
		}
		if input.TestCmd != nil {
			fields["test_cmd"] = *input.TestCmd
		}
		if input.Description != nil {
			fields["description"] = *input.Description
		}
		if input.Active != nil {
			fields["active"] = *input.Active
		}
		if err := projectStore.Update(ctx, uint(id), fields); err != nil {
			serverError(database, "update project", err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// 删除项目注册（不影响目录本身；已引用工作项保持原 target_name）。
	r.DELETE("/projects/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		if err := projectStore.Delete(ctx, uint(id)); err != nil {
			serverError(database, "delete project", err)
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"ok": true})
	})
}
