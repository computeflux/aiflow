package otel

import (
	"bytes"
	"fmt"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
)

// Entry 描述一条待记录的错误。
type Entry struct {
	System     string // api / go-worker / pycraper / pyreplay
	Level      string // error / warning / fatal，空则默认 error
	Source     string // activity 名、路由或平台名
	WorkflowID string
	RunID      string
	TraceID    string // OpenTelemetry trace id，用于打通错误与链路追踪
	Message    string
	Stack      string
	Context    string // 建议 JSON 字符串
}

// Record 将错误写入 error_logs 表。
// 尽力而为：写入失败只打日志，不向上传播，调用方无需处理返回值。
func Record(database *db.DB, e Entry) {
	if e.System == "" {
		e.System = "unknown"
	}
	if e.Level == "" {
		e.Level = "error"
	}
	if e.Message == "" {
		e.Message = "(no message)"
	}
	rec := db.ErrorLog{
		System:     e.System,
		Level:      e.Level,
		Source:     e.Source,
		WorkflowID: e.WorkflowID,
		RunID:      e.RunID,
		TraceID:    e.TraceID,
		Message:    e.Message,
		Stack:      e.Stack,
		Context:    e.Context,
	}
	if err := database.Create(&rec).Error; err != nil {
		log.Printf("[errorsink] failed to record error log: %v", err)
	}
}

// bodyWriter 包装 gin.ResponseWriter，缓冲响应体用于记录错误上下文。
type bodyWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *bodyWriter) Write(b []byte) (int, error) {
	if w.body.Len() < 2048 {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// GinMiddleware 把 API 的所有 5xx 响应记入 error_logs（system=api）。
// 需在 gin.Recovery 之前注册，这样 handler panic 产生的 500 也能被捕获。
func RecordGinMiddleware(database *db.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		bw := &bodyWriter{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
		c.Writer = bw
		c.Next()
		if status := c.Writer.Status(); status >= 500 {
			Record(database, Entry{
				System:  "api",
				Source:  c.Request.Method + " " + c.Request.URL.Path,
				TraceID: TraceIDFromGin(c),
				Message: fmt.Sprintf("HTTP %d %s %s", status, c.Request.Method, c.Request.URL.Path),
				Context: bw.body.String(),
			})
		}
	}
}
