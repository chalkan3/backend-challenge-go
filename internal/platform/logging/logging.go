// Package logging provides the structured JSON logger. Every line carries
// the correlation identifiers available at the call site (correlationId,
// messageId, transactionId, walletId, providerId) via slog attributes.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/chalkan33/wallet/internal/platform/config"
)

// New builds the JSON logger from the validated configuration.
func New(cfg *config.Config) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})
	return slog.New(h)
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
