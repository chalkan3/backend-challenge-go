// instrumentation.go adapts the observation backends to the port consumed by
// the use cases (service.Instrumentation) — an Adapter, keeping the service
// layer free of metrics- and tracing-library imports. The workers' ports are
// adapted by messagingAdapter (see messaging_adapter.go).
//
// Two backends, one port: counters go to the go-kit metric registry (the
// /metrics contract of the challenge) and spans go to OpenTelemetry. The
// split is deliberate — metrics answer "how much/how fast over time",
// traces answer "what happened to this one request".
package bootstrap

import (
	"context"

	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

type instrumentation struct {
	m *metrics.Metrics
	p *otel.Provider
}

// newInstrumentation is the fx provider for the metric+span adapter. The
// otel provider is always present in production wiring (a disabled one is
// harmless), so both backends are wired unconditionally.
func newInstrumentation(m *metrics.Metrics, p *otel.Provider) *instrumentation {
	return &instrumentation{m: m, p: p}
}

func (i *instrumentation) WagerOutcome(kind, status, failureCode, channel string) {
	i.m.WagerResults.With("kind", kind, "status", status,
		"failure_code", failureCode, "channel", channel).Add(1)
}

func (i *instrumentation) Duplicate(source string) {
	i.m.Duplicates.With("source", source).Add(1)
}

func (i *instrumentation) ReferenceOutcome(outcome string) {
	i.m.ReferenceResolved.With("outcome", outcome).Add(1)
}

func (i *instrumentation) ReconciliationDivergence() {
	i.m.ReconciliationDivergences.Add(1)
}

// StartSpan opens a child of the span active in ctx (if any) and returns a
// facade over it. The provider installs a no-op tracer when disabled, so
// this always yields a usable span and no call site needs a nil check.
func (i *instrumentation) StartSpan(ctx context.Context, name string, attrs ...service.SpanAttr) (context.Context, service.Span) {
	ctx, sp := i.p.Tracer().Start(ctx, name, otel.SpanStartOptions(attrs...))
	return ctx, &span{sp}
}

// span adapts the OpenTelemetry span to the service.Span facade. Keeping the
// facade tiny means a change in the tracing API touches only this file.
type span struct{ s otel.Span }

func (s *span) SetAttributes(attrs ...service.SpanAttr) {
	s.s.SetAttributes(otel.KeyValues(attrs...)...)
}
func (s *span) SetOutcome(status, failureCode string) { otel.RecordOutcome(s.s, status, failureCode) }
func (s *span) RecordError(err error)                 { otel.RecordError(s.s, err) }
func (s *span) End()                                  { s.s.End() }
