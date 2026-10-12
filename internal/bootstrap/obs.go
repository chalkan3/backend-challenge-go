package bootstrap

import (
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
)

// instrumentation adapts the Prometheus registry to the instrumentation
// interfaces consumed by the app services and the workers — keeping both
// layers free of metrics-library imports.
type instrumentation struct {
	m *metrics.Metrics
}

func newInstrumentation(m *metrics.Metrics) *instrumentation {
	return &instrumentation{m: m}
}

func (i *instrumentation) WagerOutcome(kind, status, failureCode, channel string) {
	i.m.WagerResults.WithLabelValues(kind, status, failureCode, channel).Inc()
}

func (i *instrumentation) Duplicate(source string) {
	i.m.Duplicates.WithLabelValues(source).Inc()
}

func (i *instrumentation) ReferenceOutcome(outcome string) {
	i.m.ReferenceResolved.WithLabelValues(outcome).Inc()
}

func (i *instrumentation) ReconciliationDivergence() {
	i.m.ReconciliationDivergences.Inc()
}

func (i *instrumentation) OutboxPublished() { i.m.OutboxPublished.Inc() }
func (i *instrumentation) OutboxFailed()    { i.m.OutboxFailures.Inc() }

func (i *instrumentation) OutboxStats(pending int64, lagSeconds float64) {
	i.m.OutboxPending.Set(float64(pending))
	i.m.OutboxLagSeconds.Set(lagSeconds)
}

func (i *instrumentation) Received()             { i.m.ConsumerReceived.Inc() }
func (i *instrumentation) Retry()                { i.m.ConsumerRetries.Inc() }
func (i *instrumentation) DLQ(reason string)     { i.m.ConsumerDLQ.Inc() }
func (i *instrumentation) Invalid(reason string) {}
