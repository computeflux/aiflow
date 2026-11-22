package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
)

// registerBoardMindmapRoutes 看板·思维导图 CRUD 与 AI 对话。逻辑自 registerBoardRoutes 原样
// 搬迁（批次 F 拆分，行为无变更）。
func registerBoardMindmapRoutes(
	r *gin.Engine,
	deps *appDeps,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database

	// Mind maps
	r.GET("/mindmaps", requireAuth(), func(ctx *gin.Context) {
		var maps []db.MindMap
		database.Order("updated_at desc").Find(&maps)
		ctx.JSON(http.StatusOK, gin.H{"mindmaps": maps})
	})

	r.POST("/mindmaps", requireAuth(), func(ctx *gin.Context) {
		var m db.MindMap
		if err := ctx.ShouldBindJSON(&m); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		database.Create(&m)
		ctx.JSON(http.StatusCreated, gin.H{"mindmap": m})
	})

	r.PUT("/mindmaps/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var input db.MindMap
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		database.Model(&db.MindMap{}).Where("id = ?", id).Updates(map[string]any{
			"name": input.Name, "data": input.Data, "layout": input.Layout,
			"theme": input.Theme, "view_x": input.ViewX, "view_y": input.ViewY,
			"scale": input.Scale, "updated_at": time.Now(),
		})
		ctx.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.DELETE("/mindmaps/:id", requireAuth(), func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&db.MindMap{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	// Mind map AI chat
	r.POST("/mindmap-chat", requireAuth(), func(ctx *gin.Context) {
		var input struct {
			MindMapJSON string `json:"mindmap_json"`
			Message     string `json:"message"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		system := "You are a precise mind map editor. The user provides a mind map in JSON format and asks you to modify it.\n\n" +
			"JSON STRUCTURE:\n" +
			"Each node is an object with: {\"data\": {\"text\": \"node content\", \"expand\": true}, \"children\": [...]}\n" +
			"- data.text: the node's display text (required)\n" +
			"- data.expand: whether children are visible, default true (optional)\n" +
			"- children: array of child nodes, omit if no children\n" +
			"- depth: nodes can be nested arbitrarily deep\n\n" +
			"MODIFICATION RULES:\n" +
			"1. Always return the COMPLETE mind map JSON, not just changes. Start from the root.\n" +
			"2. To ADD nodes: insert new child objects into the correct parent's children array.\n" +
			"3. To DELETE nodes: remove them from their parent's children array.\n" +
			"4. To RENAME nodes: change the text field only.\n" +
			"5. To RESTRUCTURE: move nodes to different parents as needed.\n" +
			"6. Preserve existing node data unless explicitly asked to change it.\n" +
			"7. Keep node texts concise — typically 2-8 words.\n" +
			"8. Organize logically: similar topics grouped under same parent.\n\n" +
			"OUTPUT: Return ONLY valid JSON. No markdown, no explanation, no code fences. Start with { and end with }."
		user := fmt.Sprintf("Current mind map JSON:\n%s\n\nUser request: %s", input.MindMapJSON, input.Message)
		response, err := deps.llm.ChatCompletion(ctx, system, user)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, serverError(database, "mindmap-chat", err))
			return
		}
		jsonStart := strings.Index(response, "{")
		jsonEnd := strings.LastIndex(response, "}")
		if jsonStart >= 0 && jsonEnd > jsonStart {
			response = response[jsonStart : jsonEnd+1]
		}
		ctx.JSON(http.StatusOK, gin.H{"reply": "Done", "mindmap_json": response})
	})
}
