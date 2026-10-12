// messaging_adapter.go adapts the background workers' observation ports to
// the two backends (go-kit metrics and OpenTelemetry). It is a separate type
// from instrumentation because Go has no method overloads: the workers'
// port speaks messaging.SpanAttr while the use cases speak service.SpanAttr,
// so each boundary gets its own adapter. Uber Fx binds the right one to each
// interface.
package bootstrap

import (
	"context"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
)

// messagingAdapter implements messaging.Instrumentation and
// messaging.ConsumerMetrics.
type messagingAdapter struct {
	m *metrics.Metrics
	p *otel.Provider
}

func newMessagingAdapter(m *metrics.Metrics, p *otel.Provider) *messagingAdapter {
	return &messagingAdapter{m: m, p: p}
}

// --- messaging.Instrumentation (outbox relay) -------------------------------

func (a *messagingAdapter) OutboxPublished() { a.m.OutboxPublished.Add(1) }
func (a *messagingAdapter) OutboxFailed()    { a.m.OutboxFailures.Add(1) }

func (a *messagingAdapter) OutboxStats(pending int64, lagSeconds float64) {
	a.m.OutboxPending.Set(float64(pending))
	a.m.OutboxLagSeconds.Set(float64(lagSeconds))
}

// --- messaging.ConsumerMetrics ----------------------------------------------

func (a *messagingAdapter) Received()         { a.m.ConsumerReceived.Add(1) }
func (a *messagingAdapter) Retry()            { a.m.ConsumerRetries.Add(1) }
func (a *messagingAdapter) DLQ(reason string) { a.m.ConsumerDLQ.Add(1) }

// Duplicate records an inbox redelivery or a lost insert race, with the
// detection source as the label.
func (a *messagingAdapter) Duplicate(source string) { a.m.Duplicates.With("source", source).Add(1) }

// Invalid records a structurally invalid payload. Poison bodies are already
// counted by ConsumerDLQ; the parameter stays for the port contract.
func (a *messagingAdapter) Invalid(reason string) { _ = reason }

// StartSpan satisfies both messaging ports (they declare the same signature).
func (a *messagingAdapter) StartSpan(ctx context.Context, name string, attrs ...messaging.SpanAttr) (context.Context, messaging.Span) {
	ctx, sp := a.p.Tracer().Start(ctx, name, otel.SpanStartOptions(attrs...))
	return ctx, &msgSpan{sp}
}

// msgSpan is the messaging-side facade; it exists separately from the
// service-side span because the two ports carry distinct attribute types and
// Go has no method overloads.
type msgSpan struct{ s otel.Span }

func (s *msgSpan) SetAttributes(attrs ...messaging.SpanAttr) {
	s.s.SetAttributes(otel.KeyValues(attrs...)...)
}
func (s *msgSpan) SetOutcome(status, failureCode string) {
	otel.RecordOutcome(s.s, status, failureCode)
}
func (s *msgSpan) RecordError(err error) { otel.RecordError(s.s, err) }
func (s *msgSpan) End()                  { s.s.End() }
