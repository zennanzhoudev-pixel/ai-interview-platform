package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TraceConfig 配置链路追踪。
type TraceConfig struct {
	// ServiceName 出现在 Jaeger 的服务列表里。
	ServiceName string
	// Endpoint 是 OTLP/HTTP 接收地址(如 127.0.0.1:4318)。留空则关闭导出。
	Endpoint string
	Insecure bool
	// SampleRatio 采样率。面试系统按场次采样更合理, 这里先用统一比例。
	SampleRatio float64
}

// InitTracing 初始化全局 TracerProvider。
//
// 返回的 shutdown 必须在进程退出前调用, 否则缓冲区里最后一批 span 会丢 ——
// 而"最后一批"往往正好是崩溃前的那几个, 恰恰是最需要看的。
func InitTracing(ctx context.Context, cfg TraceConfig) (func(context.Context) error, trace.TracerProvider, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "interviewd"
	}
	if cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}

	if cfg.Endpoint == "" {
		// 未配置端点: 用 no-op provider, 保证调用方代码零改动。
		provider := noop.NewTracerProvider()
		otel.SetTracerProvider(provider)
		return func(context.Context) error { return nil }, provider, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("observability: 初始化 OTLP 导出失败: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", "0.1.0"),
	))
	if err != nil {
		return nil, nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, provider, nil
}

// Tracer 返回具名 tracer。
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }
