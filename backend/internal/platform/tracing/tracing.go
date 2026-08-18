// Package tracing 负责 OpenTelemetry TracerProvider 的初始化与优雅关闭。
package tracing

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

// ScopeName 是全项目手动埋点统一使用的 instrumentation scope。
const ScopeName = "github.com/11bronx11/bx_yunpan/backend"

// Tracer 返回统一 scope 的 tracer。未初始化时拿到的是 no-op，调用侧无需判空。
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Shutdown 由调用方在进程退出前执行，负责 flush 未导出的 span。
type Shutdown func(context.Context) error

// Setup 初始化全局 TracerProvider 与 propagator。
//
// Enabled 为 false 时返回 no-op shutdown，且不注册全局 provider——此时
// otel.Tracer 拿到的是 no-op tracer，所有埋点开销接近于零。
func Setup(ctx context.Context, cfg config.Tracing, serviceName string) (Shutdown, error) {
	// 无论是否启用都要装 propagator：即使本进程不采样，也应把上游传下来的
	// traceparent 继续往下游传，不能在中间断链。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}

	attributes, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("deployment.environment", cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(attributes),
		// ParentBased 保证一条链路要么全采要么全不采，不会出现半截 trace。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplerRatio))),
	)
	otel.SetTracerProvider(provider)

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return provider.Shutdown(shutdownCtx)
	}, nil
}
