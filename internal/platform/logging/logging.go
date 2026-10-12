// Package logging provides the structured JSON logger. Every line carries
// the correlation identifiers available at the call site (correlationId,
// messageId, transactionId, walletId, providerId) via slog attributes.
//
// When OpenTelemetry tracing is enabled the handler is wrapped so that every
// record also carries the trace_id/span_id of the active span. That join is
// what makes a log line clickable from a trace, and it costs no change at any
// log call site — the correlation is derived from the context slog already
// receives.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
)

// New builds the JSON logger from the validated configuration. The tracing
// provider may be nil (telemetry disabled), in which case the handler is left
// untouched.
func New(cfg *config.Config, p *otel.Provider) *slog.Logger {
	return NewWithWriter(cfg, p, os.Stdout)
}

// NewWithWriter is New with an explicit sink. Tests use it to capture output
// without replacing os.Stdout, which would race with the parallel test
// scheduler.
func NewWithWriter(cfg *config.Config, p *otel.Provider, w io.Writer) *slog.Logger {
	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level(cfg.LogLevel)})
	if p != nil && p.Enabled() {
		// Fan out: stdout keeps the raw JSON contract, OTLP gets the same
		// record with trace context attached.
		h = &fanoutHandler{a: otel.NewTracingAttrsHandler(h), b: otel.OTelHandler(p.Telemetry().ServiceName)}
	} else {
		h = otel.NewTracingAttrsHandler(h)
	}
	return slog.New(h)
}

func level(l string) slog.Level {
	switch strings.ToLower(l) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// fanoutHandler writes every record to both handlers. The bridge to OTLP is
// best-effort: a failure to export a log must never break the request that
// produced it, so errors from the secondary handler are swallowed.
type fanoutHandler struct {
	a, b slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return f.a.Enabled(ctx, l) || f.b.Enabled(ctx, l)
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	err := f.a.Handle(ctx, r)
	if f.b.Enabled(ctx, r.Level) {
		_ = f.b.Handle(ctx, r)
	}
	return err
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{a: f.a.WithAttrs(attrs), b: f.b.WithAttrs(attrs)}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{a: f.a.WithGroup(name), b: f.b.WithGroup(name)}
}

// Context keys for correlation identifiers.
type ctxKey string

const (
	ctxCorrelationID ctxKey = "correlationId"
	ctxMessageID     ctxKey = "messageId"
)

// WithCorrelation returns a context carrying the correlation id, which the
// logging adapter attaches to every log line.
func WithCorrelation(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, ctxCorrelationID, correlationID)
}

// WithMessage returns a context carrying the transport message id.
func WithMessage(ctx context.Context, messageID string) context.Context {
	return context.WithValue(ctx, ctxMessageID, messageID)
}

func CorrelationFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxCorrelationID).(string)
	return v
}

func MessageFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxMessageID).(string)
	return v
}

// AttrsFrom extracts the correlation attributes present in ctx.
func AttrsFrom(ctx context.Context) []any {
	var attrs []any
	if c := CorrelationFrom(ctx); c != "" {
		attrs = append(attrs, slog.String("correlationId", c))
	}
	if m := MessageFrom(ctx); m != "" {
		attrs = append(attrs, slog.String("messageId", m))
	}
	return attrs
}
