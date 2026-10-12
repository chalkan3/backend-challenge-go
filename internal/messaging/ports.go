package messaging

// Instrumentation is the observation port of the outbox relay (Observer
// pattern). The composition root adapts it to the go-kit metric registry,
// keeping this package free of metrics-library imports.
type Instrumentation interface {
	OutboxPublished()
	OutboxFailed()
	OutboxStats(pending int64, lagSeconds float64)
}

// ConsumerMetrics is the observation port of the SQS consumer.
type ConsumerMetrics interface {
	Received()
	Duplicate(source string)
	DLQ(reason string)
	Retry()
	Invalid(reason string)
}
