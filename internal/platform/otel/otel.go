// Package otel wires OpenTelemetry into the service: one resource, a tracer
// provider, a logger provider and the W3C propagator, all owned by the Fx
// lifecycle (shutdown flushes whatever is still buffered).
//
// Design boundary — one signal, one owner:
//
//	metrics → kit/metrics + Prometheus (challenge requirement, /metrics)
//	traces  → OTLP/gRPC to the Collector
//	logs    → slog JSON on stdout (slog bridge, correlated via trace_id)
//
// Traces deliberately never duplicate the metric counters: the Collector
// derives span metrics (R.E.D.) from the spans, so each signal has exactly
// one source of truth.
//
// The zero value of Config disables export but still installs a working
// tracer, so every code path (and every unit test) can call Start without a
// Collector running. That is why instrumentation never needs a nil check.
package otel

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config is the tracing configuration, loaded from the environment by the
// config package and validated there.
type Config struct {
	Enabled        bool
	Endpoint       string // OTLP/gRPC host:port of the Collector
	Insecure       bool   // plaintext gRPC (local Collector)
	ServiceName    string
	ServiceVersion string
	Environment    string
	InstanceID     string        // resource attribute; distinguishes replicas
	SampleRatio    float64       // 1.0 = always sample
	ExportTimeout  time.Duration // per-export deadline
	BatchTimeout   time.Duration // max delay before a batch is flushed
	MaxQueueSize   int
}

// Provider bundles the SDK handles that the composition root needs: the
// tracer for manual spans, the shared resource (also used by the log
// pipeline), the effective configuration and Shutdown for the lifecycle hook.
type Provider struct {
	tp       trace.TracerProvider
	tracer   trace.Tracer
	resource *sdkresource.Resource
	cfg      Config
	shutdown func(context.Context) error
	enabled  bool
}

// Tracer returns the service tracer. It is never nil: when tracing is
// disabled the provider falls back to the no-op implementation, so callers
// (and tests) need no branching.
func (p *Provider) Tracer() trace.Tracer {
	if p == nil || p.tracer == nil {
		return noop.NewTracerProvider().Tracer("wallet")
	}
	return p.tracer
}

// TracerProvider exposes the SDK provider for integrations that need more
// than a single tracer (otelhttp, otelpgx, the OTLP log bridge). Like Tracer
// it never returns nil: the no-op provider keeps instrumentation code free
// of nil checks when telemetry is disabled.
func (p *Provider) TracerProvider() trace.TracerProvider {
	if p == nil || p.tp == nil {
		return noop.NewTracerProvider()
	}
	return p.tp
}

// Enabled reports whether spans are actually exported.
func (p *Provider) Enabled() bool { return p != nil && p.enabled }

// Telemetry returns the effective configuration, so startup logs describe
// what is actually running rather than what was requested.
func (p *Provider) Telemetry() Config {
	if p == nil {
		return Config{}
	}
	return p.cfg
}

// Shutdown flushes pending spans and stops the exporter. It is safe to call
// on a disabled or nil provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// New builds the tracer provider from cfg. It never fails hard on an
// unreachable Collector: the OTLP/gRPC exporter reconnects in the background,
// so a missing observability stack degrades to dropped spans instead of a
// dead application (availability over telemetry).
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{enabled: false, cfg: cfg}, nil
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithTimeout(cfg.ExportTimeout),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(clampRatio(cfg.SampleRatio)))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(cfg.BatchTimeout),
			sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
		),
	)

	return &Provider{
		tp:       tp,
		tracer:   tp.Tracer(cfg.ServiceName),
		resource: res,
		cfg:      cfg,
		shutdown: tp.Shutdown,
		enabled:  true,
	}, nil
}

// newResource describes the emitting process. service.instance.id and
// service.version make three replicas distinguishable in the trace UI; the
// schema URL keeps the attribute set versioned.
//
// The semconv version must match the one the SDK's own detectors use:
// sdkresource.Merge rejects resources with conflicting Schema URLs, and a
// mismatch aborts startup. TestNewEnabledMatchesSDKSemconv pins that.
func newResource(_ context.Context, cfg Config) (*sdkresource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(cfg.InstanceID))
	}
	res, err := sdkresource.Merge(
		sdkresource.Default(), // process/host/telemetry.sdk.*
		sdkresource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// clampRatio keeps the sampler ratio inside [0,1]; a misconfigured ratio
// must not silently drop every span.
func clampRatio(r float64) float64 {
	switch {
	case r <= 0:
		return 1.0 // "0" almost always means "not configured", not "sample nothing"
	case r > 1:
		return 1.0
	default:
		return r
	}
}

// ErrNoEndpoint guards a configuration mistake: enabling tracing without an
// endpoint would silently blackhole every span.
var ErrNoEndpoint = errors.New("otel: tracing enabled but endpoint is empty")

// Validate is called by the config package at startup so a broken
// observability configuration fails fast, like every other dependency.
func (c Config) Validate() error {
	if c.Enabled && c.Endpoint == "" {
		return ErrNoEndpoint
	}
	return nil
}

// LogAttrs exposes the effective telemetry identity for the startup log line.
func (c Config) LogAttrs() []any {
	return []any{
		slog.Bool("enabled", c.Enabled),
		slog.String("endpoint", c.Endpoint),
		slog.String("service", c.ServiceName),
		slog.String("instance", c.InstanceID),
		slog.Float64("sampleRatio", c.SampleRatio),
	}
}
