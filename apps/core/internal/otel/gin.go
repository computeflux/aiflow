package otel

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traceIDContextKey 是写入 gin.Context 的 trace id 键。
// errorsink 通过同一键读取（避免 errorsink 反向依赖 otel 包内细节以外的约定）。
const traceIDContextKey = "otel.trace.id"

// GinMiddleware 为每个 HTTP 请求开启一个 span（SpanKindServer），
// 并把 trace id 写入 gin.Context（键 traceIDContextKey）与响应头 X-Trace-Id。
// 注册位置：必须在业务 handler 与 errorsink.GinMiddleware 之前。
func GinMiddleware() gin.HandlerFunc {
	tracer := otel.Tracer(tracerName)
	propagators := otel.GetTextMapPropagator()

	return func(c *gin.Context) {
		// 从请求头提取 traceparent，与上游服务串成一条链路
		ctx := propagators.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path // 未匹配路由时 FullPath 为空
		}
		ctx, span := tracer.Start(ctx, "http."+c.Request.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		c.Request = c.Request.WithContext(ctx)
		traceID := span.SpanContext().TraceID().String()
		c.Set(traceIDContextKey, traceID)
		c.Header("X-Trace-Id", traceID)

		c.Next()

		span.SetAttributes(attribute.Int("http.status_code", c.Writer.Status()))
		if c.Writer.Status() >= 400 {
			span.SetStatus(codes.Error, "")
		}
	}
}

// TraceIDFromGin 从 gin.Context 读取中间件写入的 trace id，无则返回空串。
func TraceIDFromGin(c *gin.Context) string {
	if v, ok := c.Get(traceIDContextKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
