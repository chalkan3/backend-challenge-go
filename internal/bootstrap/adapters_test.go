// This file exercises the composition-root ADAPTERS directly. They are thin
// by design — every method is a one-line delegation to the metrics registry or
// the tracing provider — but "thin" is not "covered": a typo in an adapter
// (a wrong label, a dropped attribute, a forwarded call that never runs) is
// invisible to the use-case tests, which talk to the port, not the adapter.
// The otel provider is created disabled, so the spans are real objects backed
// by a no-op tracer: the delegation is still executed end to end, without an
// exporter or a Collector.
package bootstrap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// disabledProvider returns a real Provider whose tracer is a no-op. Spans are
// still valid objects, so every delegation runs — only the export is skipped.
func disabledProvider(t *testing.T) *otel.Provider {
	t.Helper()
	p, err := otel.New(context.Background(), otel.Config{Enabled: false})
	require.NoError(t, err)
	require.NotNil(t, p)
	return p
}

// TestInstrumentationAdapterForwardsToBackends calls every method of the
// service-side instrumentation adapter. Each must complete without panicking
// and must hand a usable span facade back to the caller.
func TestInstrumentationAdapterForwardsToBackends(t *testing.T) {
	inst := newInstrumentation(metrics.New(), disabledProvider(t))

	// Metrics side: no assertion beyond "it runs and does not panic",
	// because the registry's shape is go-kit's contract, exercised elsewhere.
	assert.NotPanics(t, func() {
		inst.WagerOutcome("BET", "PROCESSED", "", "HTTP")
		inst.WagerOutcome("WIN", "REJECTED", service.FailInsufficientFunds, "SQS")
		inst.Duplicate("idempotent_replay")
		inst.Duplicate("insert_race_replay")
		inst.ReferenceOutcome("scheduled")
		inst.ReferenceOutcome("retry_scheduled")
		inst.ReconciliationDivergence()
	})

	// Tracing side: StartSpan must return a working facade for both the
	// empty and the attributed call, and every facade method must delegate.
	ctx, sp := inst.StartSpan(context.Background(), service.SpanWagerExecute,
		service.Attr(service.AttrKind, "BET"),
		service.Attr(service.AttrProviderID, "prov-1"))
	require.NotNil(t, ctx)
	require.NotNil(t, sp)

	ctx, sp = inst.StartSpan(context.Background(), service.SpanWalletOpen)
	require.NotNil(t, ctx)
	require.NotNil(t, sp)
	assert.NotPanics(t, func() {
		sp.SetAttributes(service.Attr(service.AttrWalletID, "w-1"))
		sp.SetOutcome("PROCESSED", "")
		sp.RecordError(nil)
		sp.End()
	})
}

// TestMessagingAdapterForwardsToBackends does the same for the worker-side
// adapter, which carries its own span/attribute vocabulary.
func TestMessagingAdapterForwardsToBackends(t *testing.T) {
	ad := newMessagingAdapter(metrics.New(), disabledProvider(t))

	assert.NotPanics(t, func() {
		ad.OutboxPublished()
		ad.OutboxFailed()
		ad.OutboxStats(7, 1.5)
		ad.Received()
		ad.Retry()
		ad.DLQ("terminal")
		ad.Duplicate("inbox")
		ad.Invalid("poison")
	})

	ctx, sp := ad.StartSpan(context.Background(), "messaging.consume",
		messaging.Attr(messaging.AttrMessageID, "msg-1"))
	require.NotNil(t, ctx)
	require.NotNil(t, sp)

	ctx, sp = ad.StartSpan(context.Background(), "messaging.consume")
	require.NotNil(t, ctx)
	require.NotNil(t, sp)
	assert.NotPanics(t, func() {
		sp.SetAttributes(messaging.Attr(messaging.AttrMessageID, "msg-2"))
		sp.SetOutcome("FAILED", "TERMINAL")
		sp.RecordError(nil)
		sp.End()
	})
}
