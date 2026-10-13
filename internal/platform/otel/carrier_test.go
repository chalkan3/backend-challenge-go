package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestCarrierRoundTripsTraceContext is the core contract: a traceparent
// produced on one side must reconstruct the SAME trace (same trace_id) on the
// other side after travelling as a plain string through the outbox row.
func TestCarrierRoundTripsTraceContext(t *testing.T) {
	t.Parallel()

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	c := NewCarrier()

	// Producer side: an active, sampled span (as the HTTP server would have).
	pctx, producer := tp.Tracer("producer").Start(context.Background(), "wallet.http.submit_transaction")
	require.True(t, producer.SpanContext().IsValid(), "producer span must be valid")
	require.True(t, producer.SpanContext().IsSampled(), "producer span must be sampled to be propagated")

	carrier := c.Inject(pctx)
	require.NotEmpty(t, carrier, "a sampled span must yield a traceparent")

	// Consumer side: extract from the bare string, then open a child.
	cctx := c.Extract(context.Background(), carrier)
	_, child := tp.Tracer("consumer").Start(cctx, "wallet.sqs.consume")
	defer child.End()

	assert.Equal(t, producer.SpanContext().TraceID(), child.SpanContext().TraceID(),
		"the consumer span must belong to the producer's trace, not a new one")
	assert.NotEqual(t, producer.SpanContext().SpanID(), child.SpanContext().SpanID(),
		"the child must be a distinct span")
	// The W3C carrier names the producer as the remote parent; the SDK wires
	// that into the child's parent span context.
	assert.Equal(t, producer.SpanContext().SpanID(), trace.SpanContextFromContext(cctx).SpanID(),
		"the extracted context must name the producer span as the remote parent")
}

// TestCarrierEmptyAndGarbageDegradeToNewTrace pins the failure mode: a missing
// or malformed carrier must never abort the work, only drop the link.
func TestCarrierEmptyAndGarbageDegradeToNewTrace(t *testing.T) {
	t.Parallel()

	c := NewCarrier()
	base := trace.SpanContextFromContext(context.Background())
	require.False(t, base.IsValid(), "precondition: no span in a bare context")

	for _, in := range []string{"", "not-a-traceparent", "00-xyz-abc-01", "1"} {
		got := c.Extract(context.Background(), in)
		assert.False(t, trace.SpanContextFromContext(got).IsValid(),
			"carrier %q must not fabricate a span context", in)
	}
}

// TestCarrierInjectWithoutSpanIsEmpty documents the no-upstream case: a bare
// API call (no incoming traceparent) injects "" and the relay starts a fresh
// trace. This is a normal state, not an error.
func TestCarrierInjectWithoutSpanIsEmpty(t *testing.T) {
	t.Parallel()

	c := NewCarrier()
	assert.Empty(t, c.Inject(context.Background()),
		"no active span must yield an empty carrier")
}

// TestNilCarrierIsUsable guards the compiler-generated nil receiver that can
// appear when the carrier is wired as a typed-nil interface: it must degrade
// to a no-op instead of panicking inside a transaction.
func TestNilCarrierIsUsable(t *testing.T) {
	t.Parallel()

	var c *Carrier
	assert.Empty(t, c.Inject(context.Background()))
	assert.NotNil(t, c.Extract(context.Background(), "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"))
}
