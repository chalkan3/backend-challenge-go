// logs.go bridges slog to OpenTelemetry without moving any log call site.
//
// The service keeps emitting structured JSON on stdout (what `docker compose
// logs` and the integration tests read), and the same records are forwarded
// to the Collector when OTLP log export is enabled. Correlation is implicit:
// every record written through the OTel-aware handler carries trace_id and
// span_id of the active span, so a log line and its trace are joined by the
// backend instead of by a hand-rolled correlation id.
package otel

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// TracingAttrsHandler decorates a slog.Handler, adding the trace context of
// the record's context as attributes. Because slog passes the context into
// Handle, the ids come from the exact span that emitted the line — no global
// "current span" lookup.
type TracingAttrsHandler struct {
	inner slog.Handler
}

// NewTracingAttrsHandler wraps inner so each record gains trace_id/span_id
// when the record's context carries an active span.
func NewTracingAttrsHandler(inner slog.Handler) *TracingAttrsHandler {
	return &TracingAttrsHandler{inner: inner}
}

func (h *TracingAttrsHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *TracingAttrsHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *TracingAttrsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TracingAttrsHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *TracingAttrsHandler) WithGroup(name string) slog.Handler {
	return &TracingAttrsHandler{inner: h.inner.WithGroup(name)}
}

// NewLoggerProvider builds the OTLP log pipeline on the same resource as the
// tracer provider, so logs and spans resolve to one service instance. It
// returns nil when log export is disabled, which the caller reads as
// "stdout only".
func (p *Provider) NewLoggerProvider(ctx context.Context, exp sdklog.Exporter) (*sdklog.LoggerProvider, error) {
	if !p.Enabled() || exp == nil {
		return nil, nil
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(p.resource),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	global.SetLoggerProvider(lp)
	return lp, nil
}

// OTelHandler returns an slog handler that forwards records to the global
// OTel logger provider (installed by NewLoggerProvider). It never returns
// nil, so it can be composed unconditionally.
func OTelHandler(serviceName string) slog.Handler {
	return otelslog.NewHandler(serviceName)
}

// otelLoggerName is the instrumentation scope of the bridged log records.
const otelLoggerName = "wallet"

var _ otellog.Logger = global.GetLoggerProvider().Logger(otelLoggerName)
