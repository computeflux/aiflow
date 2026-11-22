// Package main is the flex-server binary.
//
// Command flex-server 是 S2 柔性分支图引擎服务（docs/plan/S2-混合编排.md §3.3）：
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wetee/ai-workflow/internal/flex"
	_ "github.com/wetee/ai-workflow/internal/flex/graphs"
)

// S2 §3.3 硬约束（写死代码，不可配置；runTimeout 为 var 仅为单测可缩短）
const (
	maxNodes       = 20
	maxInputTokens = 50_000
)

var runTimeout = 10 * time.Minute

type runRequest struct {
	Graph       string         `json:"graph"`
	Input       map[string]any `json:"input"`
	Traceparent string         `json:"traceparent,omitempty"`
}

func main() {
	port := os.Getenv("FLEX_SERVER_PORT")
	if port == "" {
		port = "8124"
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.POST("/runs", handleRun)

	srv := &http.Server{Addr: ":" + port, Handler: r}
	go func() {
		log.Printf("flex-server listening on :%s (graphs: %v)", port, flex.List())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func handleRun(c *gin.Context) {
	// 请求体硬限 1MB（token 上限 50k ≈ 200KB JSON；防止无界 body 解析吃内存）
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var req runRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}

	def, ok := flex.Get(req.Graph)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"detail": "unknown graph: " + req.Graph})
		return
	}

	// 硬约束 1：输入 token
	body, _ := json.Marshal(req.Input)
	tokens := len(body) / 4
	if tokens > maxInputTokens {
		c.JSON(http.StatusBadRequest, gin.H{"detail": fmt.Sprintf("input too large: ~%d tokens > %d", tokens, maxInputTokens)})
		return
	}

	// 硬约束 2：节点数
	if n := def.Graph.NodeCount(); n > maxNodes {
		c.JSON(http.StatusBadRequest, gin.H{"detail": fmt.Sprintf("graph too large: %d nodes > %d", n, maxNodes)})
		return
	}

	// 入参 JSON Schema 校验
	if err := flex.ValidateSchema(def.InSchema, req.Input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "input schema validation failed: " + err.Error()})
		return
	}

	log.Printf("run graph=%s nodes=%d input_tokens~%d traceparent=%s", req.Graph, def.Graph.NodeCount(), tokens, req.Traceparent)
	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), runTimeout)
	defer cancel()
	state, err := def.Graph.Invoke(ctx, flex.State(req.Input))
	latency := time.Since(start)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("run graph=%s timed out after %v", req.Graph, runTimeout)
			c.JSON(http.StatusGatewayTimeout, gin.H{"detail": "run timed out after 10m (hard limit)"})
			return
		}
		log.Printf("run graph=%s failed: %v", req.Graph, err)
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "graph execution failed: " + err.Error()})
		return
	}

	// 出参 JSON Schema 校验（尽力而为：失败仅告警，不拒绝）
	if err := flex.ValidateSchema(def.OutSchema, state); err != nil {
		log.Printf("WARN: output schema validation failed for graph=%s: %v", req.Graph, err)
	}

	log.Printf("run graph=%s ok latency=%v", req.Graph, latency)
	c.JSON(http.StatusOK, gin.H{"ok": true, "output": state})
}
