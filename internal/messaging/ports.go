package messaging

import "context"

// Instrumentation is the observation port of the outbox relay (Observer
// pattern). The composition root adapts it to the go-kit metric registry,
// keeping this package free of metrics-library imports.
type Instrumentation interface {
	OutboxPublished()
	OutboxFailed()
	OutboxStats(pending int64, lagSeconds float64)

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
)
