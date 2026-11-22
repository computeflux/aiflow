// Package otel 是观测域统一入口：OpenTelemetry 链路追踪的初始化与便捷封装，并含统一错误落库
// （error_logs 表写入入口 Record；原 internal/errorsink，批次 G 并入）。
//
// 各服务（workflow-api / go-worker）在 main 中调用 Init 完成 TracerProvider 初始化，
// 上报到 OTLP endpoint（默认 http://127.0.0.1:4317，otel-collector）。
package otel

import (
	"context"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracerName 是本模块所有 span 使用的 tracer 名。
const tracerName = "github.com/wetee/ai-workflow"

// Init 初始化 TracerProvider 并注册全局 propagator。
// 返回的 shutdown 需在 main 的 defer 中调用，用于 flush 未上报的 span。
//
// 环境变量：
//   - OTEL_EXPORTER_OTLP_ENDPOINT：OTLP endpoint（默认 http://127.0.0.1:4317）
//   - OTEL_SAMPLING_RATIO：采样率 0~1（默认 1.0 全量，调试期推荐）
func Init(ctx context.Context, serviceName string) (func(context.Context), error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:4317"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpointURL(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
	)
	if err != nil {
		return nil, err
	}

	sampling := 1.0
	if v := os.Getenv("OTEL_SAMPLING_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			sampling = f
		}
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampling))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func(ctx context.Context) {
		// 给 flush 一个上限，避免 exporter 断开时关停卡死
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}, nil
}

// Start 开启一个 span；ctx 无父 span 时自动成为 root span。
// 调用方负责 defer span.End()。
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// TraceIDFrom 返回 ctx 中 span 的 trace id（32 位 hex），无有效 span 时返回空串。
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}
