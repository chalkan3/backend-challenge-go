// Package metrics declares the service metrics as go-kit metric
// primitives (kit/metrics) backed by the Prometheus client, all registered
// on a per-process registry so tests and multiple Fx apps never collide.
//
// Every field is a kit metrics.Counter/Gauge/Histogram — including the
// labeled ones, which are kitprometheus wrappers over the corresponding
// vectors. Callers stay decoupled from Prometheus and label sets are
// attached at the call site via With(labelValues...).
package metrics

import (
	"github.com/go-kit/kit/metrics"
	kitprometheus "github.com/go-kit/kit/metrics/prometheus"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
)

// Metrics is the metric registry of the service. Field names mirror the
// Prometheus series names documented in README §observability.
type Metrics struct {
	registry *stdprometheus.Registry

	// HTTP endpoint metrics (go-kit endpoint middleware — Decorator).
	RequestCount   metrics.Counter   // endpoint, success, error
	RequestLatency metrics.Histogram // endpoint

	// Domain metrics (adapted to service/messaging instrumentation ports).
	WagerResults              metrics.Counter // kind, status, failure_code, channel
	Duplicates                metrics.Counter // source
	OutboxPublished           metrics.Counter
	OutboxFailures            metrics.Counter
	OutboxPending             metrics.Gauge
	OutboxLagSeconds          metrics.Gauge
	ConsumerReceived          metrics.Counter
	ConsumerRetries           metrics.Counter
	ConsumerDLQ               metrics.Counter
	ReconciliationDivergences metrics.Counter
	ReconciliationChecked     metrics.Counter
	ReconciliationSweeps      metrics.Counter
	AuthFailures              metrics.Counter // reason
	ReferenceResolved         metrics.Counter // outcome
}

// New builds the metric set. The Prometheus collectors are created here and
// registered explicitly (kit's NewCounterFrom auto-registers on the global
// registry, which breaks multiple Fx apps in one process).
func New() *Metrics {
	reg := stdprometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		RequestCount: newCounter(reg, "endpoint", "requests_total",
			"Total requests handled, by endpoint and outcome.", "endpoint", "success", "error"),
		RequestLatency: newHistogram(reg, "endpoint", "request_duration_seconds",
			"Request latency in seconds.", []string{"endpoint"}),
		WagerResults: newCounter(reg, "wager", "results_total",
			"Wager submissions by kind, terminal status, failure code and channel.",
			"kind", "status", "failure_code", "channel"),
		Duplicates: newCounter(reg, "wager", "duplicates_total",
			"Idempotent replays by detection source.", "source"),
		OutboxPublished: newCounter(reg, "outbox", "published_total",
			"Integration events successfully published."),
		OutboxFailures: newCounter(reg, "outbox", "failures_total",
			"Outbox publish attempts that failed and will be retried."),
		OutboxPending: newGauge(reg, "outbox", "pending",
			"Outbox rows not yet published."),
		OutboxLagSeconds: newGauge(reg, "outbox", "lag_seconds",
			"Age of the oldest unpublished outbox row."),
		ConsumerReceived: newCounter(reg, "consumer", "received_total",
			"SQS messages received by the wager consumer."),
		ConsumerRetries: newCounter(reg, "consumer", "retries_total",
			"Messages released for redelivery after transient failures."),
		ConsumerDLQ: newCounter(reg, "consumer", "dlq_total",
			"Messages moved to the dead-letter queue."),
		ReconciliationDivergences: newCounter(reg, "reconciliation", "divergences_total",
			"Reconciliations that found stored balance != ledger balance."),
		ReconciliationChecked: newCounter(reg, "reconciliation", "checked_total",
			"Wallets whose stored balance was compared against the ledger."),
		ReconciliationSweeps: newCounter(reg, "reconciliation", "sweeps_total",
			"Background reconciliation passes that completed."),
		AuthFailures: newCounter(reg, "auth", "failures_total",
			"Authentication/authorization failures by reason.", "reason"),
		ReferenceResolved: newCounter(reg, "reference", "resolved_total",
			"Pending-reference resolutions by outcome.", "outcome"),
	}
	return m
}

// Registry exposes the Prometheus registry for the /metrics handler.
func (m *Metrics) Registry() *stdprometheus.Registry { return m.registry }

// newCounter builds a kit counter registered on reg (Factory helpers).
func newCounter(reg *stdprometheus.Registry, subsystem, name, help string, labels ...string) metrics.Counter {
	vec := stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
		Namespace: "wallet", Subsystem: subsystem, Name: name, Help: help,
	}, labels)
	reg.MustRegister(vec)
	return kitprometheus.NewCounter(vec)
}

// newGauge builds a kit gauge registered on reg.
func newGauge(reg *stdprometheus.Registry, subsystem, name, help string) metrics.Gauge {
	vec := stdprometheus.NewGaugeVec(stdprometheus.GaugeOpts{
		Namespace: "wallet", Subsystem: subsystem, Name: name, Help: help,
	}, nil)
	reg.MustRegister(vec)
	return kitprometheus.NewGauge(vec)
}

// newHistogram builds a kit histogram registered on reg.
func newHistogram(reg *stdprometheus.Registry, subsystem, name, help string, labels []string) metrics.Histogram {
	vec := stdprometheus.NewHistogramVec(stdprometheus.HistogramOpts{
		Namespace: "wallet", Subsystem: subsystem, Name: name, Help: help,
		Buckets: stdprometheus.ExponentialBuckets(0.001, 2, 14),
	}, labels)
	reg.MustRegister(vec)
	return kitprometheus.NewHistogram(vec)
}
