package messaging

import "context"

// Instrumentation is the observation port of the outbox relay (Observer
// pattern). The composition root adapts it to the go-kit metric registry,
// keeping this package free of metrics-library imports.
type Instrumentation interface {
	OutboxPublished()
	OutboxFailed()
	OutboxStats(pending int64, lagSeconds float64)

	// ReconciliationSweep reports a completed background pass: how many
	// wallets were compared. Divergences are NOT reported here — the use case
	// counts each one through service.Instrumentation.ReconciliationDivergence,
	// which both the on-demand route and this sweep share, so counting them
	// again would double every mismatch the sweep finds.
	ReconciliationSweep(checked int)

	// StartSpan opens a child span for one relay tick and returns the
	// derived context plus a Span façade. It never returns a nil Span.
	StartSpan(ctx context.Context, name string, attrs ...SpanAttr) (context.Context, Span)
}

// ConsumerMetrics is the observation port of the SQS consumer.
type ConsumerMetrics interface {
	Received()
	Duplicate(source string)
	DLQ(reason string)
	Retry()
	Invalid(reason string)

	// StartSpan opens a child span per handled message.
	StartSpan(ctx context.Context, name string, attrs ...SpanAttr) (context.Context, Span)
}

// TraceExtractor restores the trace context a producer forwarded with the
// message, so the consumer's span continues the originating trace instead of
// starting an unrelated one. It is the read half of service.SpanCarrier: the
// producer side (Inject) is only needed by the SQL adapter, so the consumer
// deliberately depends on the smaller interface.
type TraceExtractor interface {
	// Extract returns a context whose active span is the remote parent
	// described by carrier. An empty or malformed carrier yields ctx
	// unchanged (a new trace, never a failure).
	Extract(ctx context.Context, carrier string) context.Context
}

// SpanAttr is one attribute on a span. Each package speaks its own tiny
// vocabulary so neither imports the tracing SDK; the adapter translates.
type SpanAttr struct {
	Key string
	Val any
}

// AttrKey/AttrVal expose the pair structurally for the adapter.
func (a SpanAttr) AttrKey() string { return a.Key }
func (a SpanAttr) AttrVal() any    { return a.Val }

// Attr is the terse constructor used at call sites.
func Attr(key string, val any) SpanAttr { return SpanAttr{Key: key, Val: val} }

// Span is the minimal tracing façade used by the workers: annotate an
// outcome, record a fault, close the span.
type Span interface {
	SetAttributes(attrs ...SpanAttr)
	SetOutcome(status, failureCode string)
	RecordError(err error)
	End()
}

// Span names of the background workers.
const (
	SpanConsumerHandle = "wallet.sqs.consume"
	SpanOutboxTick     = "wallet.outbox.tick"
	SpanOutboxPublish  = "wallet.outbox.publish"
	SpanReferenceTick  = "wallet.worker.reference_tick"
	SpanReconcileSweep = "wallet.worker.reconcile_sweep"
)

// Attribute keys reported by the workers.
const (
	AttrQueue         = "wallet.queue"
	AttrMessageID     = "wallet.message_id"
	AttrOutcome       = "wallet.outcome"
	AttrDuplicate     = "wallet.idempotent_replay"
	AttrDuplicateSrc  = "wallet.duplicate_source"
	AttrOutboxBatch   = "wallet.outbox.batch"
	AttrOutboxPending = "wallet.outbox.pending"
	AttrReferenceSize = "wallet.reference.batch"
	AttrReconcileSize = "wallet.reconcile.batch"
	AttrDivergences   = "wallet.reconcile.divergences"
)
