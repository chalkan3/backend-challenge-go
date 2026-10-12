// Package metrics wires go-kit metrics over the Prometheus registry.
// Endpoint metrics use kit/metrics counters and histograms; worker gauges
// use the Prometheus client directly.
package metrics

import (
	"github.com/go-kit/kit/metrics"
	kitprometheus "github.com/go-kit/kit/metrics/prometheus"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	registry *stdprometheus.Registry

	// HTTP endpoint metrics (go-kit endpoint middleware).
	RequestCount   metrics.Counter
	RequestLatency metrics.Histogram

	// Domain counters.
	WagerResults              *stdprometheus.CounterVec // kind, status, failure_code, channel
	Duplicates                *stdprometheus.CounterVec // source
	OutboxPublished           stdprometheus.Counter
	OutboxFailures            stdprometheus.Counter
	OutboxPending             stdprometheus.Gauge
	OutboxLagSeconds          stdprometheus.Gauge
	ConsumerReceived          stdprometheus.Counter
	ConsumerRetries           stdprometheus.Counter
	ConsumerDLQ               stdprometheus.Counter
	ReconciliationDivergences stdprometheus.Counter
	AuthFailures              *stdprometheus.CounterVec // reason
	ReferenceResolved         *stdprometheus.CounterVec // outcome
}

func New() *Metrics {
	reg := stdprometheus.NewRegistry() // per-process registry: tests and
	// multiple Fx apps never collide

	// go-kit's NewCounterFrom/NewHistogramFrom auto-register on the global
	// registry, which breaks multiple Fx apps in one process (fx tests).
	// We build the collectors ourselves and register them on our own registry.
	requestCounter := stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
		Namespace: "wallet", Subsystem: "endpoint", Name: "requests_total",
		Help: "Total requests handled, by endpoint and outcome.",
	}, []string{"endpoint", "success", "error"})
	requestLatency := stdprometheus.NewHistogramVec(stdprometheus.HistogramOpts{
		Namespace: "wallet", Subsystem: "endpoint", Name: "request_duration_seconds",
		Help:    "Request latency in seconds.",
		Buckets: stdprometheus.ExponentialBuckets(0.001, 2, 14),
	}, []string{"endpoint"})

	m := &Metrics{
		RequestCount:   kitprometheus.NewCounter(requestCounter),
		RequestLatency: kitprometheus.NewHistogram(requestLatency),
		WagerResults: stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "wager", Name: "results_total",
			Help: "Wager operation outcomes by kind, status, failure code and channel.",
		}, []string{"kind", "status", "failure_code", "channel"}),
		Duplicates: stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
			Namespace: "wallet", Name: "duplicates_total",
			Help: "Duplicate deliveries detected (idempotent replays and inbox hits).",
		}, []string{"source"}),
		OutboxPublished: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "outbox", Name: "published_total",
			Help: "Outbox events successfully published.",
		}),
		OutboxFailures: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "outbox", Name: "publish_failures_total",
			Help: "Outbox publish attempts that failed and were scheduled for retry.",
		}),
		OutboxPending: stdprometheus.NewGauge(stdprometheus.GaugeOpts{
			Namespace: "wallet", Subsystem: "outbox", Name: "pending",
			Help: "Unpublished outbox rows right now.",
		}),
		OutboxLagSeconds: stdprometheus.NewGauge(stdprometheus.GaugeOpts{
			Namespace: "wallet", Subsystem: "outbox", Name: "lag_seconds",
			Help: "Age of the oldest unpublished outbox event.",
		}),
		ConsumerReceived: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "consumer", Name: "received_total",
			Help: "SQS messages received by the consumer.",
		}),
		ConsumerRetries: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "consumer", Name: "retries_total",
			Help: "Messages released for redelivery after transient failures.",
		}),
		ConsumerDLQ: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "consumer", Name: "dlq_total",
			Help: "Messages moved to the dead-letter queue.",
		}),
		ReconciliationDivergences: stdprometheus.NewCounter(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "reconciliation", Name: "divergences_total",
			Help: "Reconciliations that found stored balance != ledger balance.",
		}),
		AuthFailures: stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "auth", Name: "failures_total",
			Help: "Authentication/authorization failures by reason.",
		}, []string{"reason"}),
		ReferenceResolved: stdprometheus.NewCounterVec(stdprometheus.CounterOpts{
			Namespace: "wallet", Subsystem: "reference", Name: "resolved_total",
			Help: "Pending-reference resolutions by outcome.",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		requestCounter, requestLatency,
		m.WagerResults, m.Duplicates,
		m.OutboxPublished, m.OutboxFailures, m.OutboxPending, m.OutboxLagSeconds,
		m.ConsumerReceived, m.ConsumerRetries, m.ConsumerDLQ,
		m.ReconciliationDivergences, m.AuthFailures, m.ReferenceResolved,
	)
	return m
}

// Registry exposes the Prometheus registry for the /metrics handler.
func (m *Metrics) Registry() *stdprometheus.Registry { return m.registry }
