// carrier.go makes trace context transferable as a string, so it can survive
// a boundary that cannot carry a context.Context — the integration_outbox row
// and the SQS message that outlives the HTTP request that produced it.
//
// It uses the W3C TraceContext propagator explicitly rather than the global
// one: the global is process state that any dependency can overwrite, and a
// trace that silently changed format because a library registered something
// is exactly the kind of coupling the rest of this package avoids. Naming the
// format here makes it a deliberate, testable contract (the same reason the
// propagator is set on the provider).
package otel

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

// Carrier implements service.SpanCarrier over the W3C traceparent format.
//
// The zero value is usable and never nil-panics: the propagator is stateless,
// and an empty carrier round-trips to an unchanged context.
type Carrier struct {
	prop propagation.TextMapPropagator
}

// NewCarrier returns a carrier bound to the W3C TraceContext propagator.
func NewCarrier() *Carrier {
	return &Carrier{prop: propagation.TraceContext{}}
}

// carrierHeader is the single key this carrier reads and writes. TraceContext
// also owns tracestate (optional vendor data); deliberately carrying only the
// traceparent keeps the hop lossless for the parent link while avoiding a
// second column: tracestate is an optimisation, not the identity of the trace.
const carrierHeader = "traceparent"

// Inject renders the span active in ctx as a W3C traceparent, or "" when ctx
// carries no valid span context. An empty result is not an error: the relay
// treats it as "start a new trace".
func (c *Carrier) Inject(ctx context.Context) string {
	if c == nil || c.prop == nil {
		return ""
	}
	out := propagation.MapCarrier{}
	c.prop.Inject(ctx, out)
	return out.Get(carrierHeader)
}

// Extract returns a context whose active span context is the remote parent
// described by carrier. An empty or malformed carrier yields ctx unchanged,
// so a missing link degrades to a fresh trace instead of failing the work.
func (c *Carrier) Extract(ctx context.Context, carrier string) context.Context {
	if c == nil || c.prop == nil || carrier == "" {
		return ctx
	}
	in := propagation.MapCarrier{carrierHeader: carrier}
	return c.prop.Extract(ctx, in)
}

// compile-time guard: the carrier is exactly what the service port expects.
var _ interface {
	Inject(context.Context) string
	Extract(context.Context, string) context.Context
} = (*Carrier)(nil)
