// telemetry.go is the composition root of OpenTelemetry: it builds the tracer
// provider, optionally mirrors the structured log stream to OTLP, exposes a
// SQL tracer for the pool and flushes everything on shutdown via the Fx
// lifecycle.
//
// Ordering matters and Fx enforces it: newLoggerProvider depends on the tracer
// provider, so the logger is built after tracing exists. On stop the providers
// shut down in reverse, after the HTTP server and the workers have drained, so
// spans emitted while draining are still exported.
package bootstrap

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
)

// newTelemetryProvider builds the tracer provider. A missing or unreachable
// Collector is deliberately not fatal: the OTLP/gRPC exporter retries in the
// background, so telemetry degrades instead of taking the service down.
//
// It takes no logger on purpose. The logger itself needs the provider
// (to stamp trace_id on every line), so asking for one here would close a
// dependency cycle; the startup line is emitted by registerTelemetry, which
// Fx orders after both exist.
func newTelemetryProvider(cfg *config.Config) (*otel.Provider, error) {
	return otel.New(context.Background(), cfg.Telemetry())
}

// newLoggerProvider bridges slog to OTLP when log export is enabled. It
// returns nil (a valid, no-op dependency for Fx) when tracing or log export is
// off, which is the common local/CI case.
func newLoggerProvider(cfg *config.Config, p *otel.Provider) (*sdklog.LoggerProvider, error) {
	if !cfg.TelemetryEnabled || !cfg.TelemetryLogsEnabled {
		return nil, nil
	}
	exp, err := otlploggrpc.New(context.Background(),
		otlploggrpc.WithEndpoint(cfg.TelemetryEndpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	return p.NewLoggerProvider(context.Background(), exp)
}

// registerTelemetry announces the effective telemetry identity once tracing
// exists and flushes the exporters on shutdown. Logs go first so that
// anything logged while flushing spans is still recorded.
func registerTelemetry(lc fx.Lifecycle, p *otel.Provider, lp *sdklog.LoggerProvider, log *slog.Logger) {
	log.Info("telemetry configured", p.Telemetry().LogAttrs()...)
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			if lp != nil {
				if err := lp.Shutdown(ctx); err != nil {
					log.Warn("log provider shutdown failed", slog.String("error", err.Error()))
				}
			}
			if err := p.Shutdown(ctx); err != nil {
				log.Warn("trace provider shutdown failed", slog.String("error", err.Error()))
				return nil
			}
			if p.Enabled() {
				log.Info("telemetry flushed")
			}
			return nil
		},
	})
}
