// instrumentation.go adapts the go-kit metric registry to the observation
// ports consumed by the use cases (service.Instrumentation) and the
// background workers (messaging.Instrumentation, messaging.ConsumerMetrics)
// — an Adapter per port, keeping every other layer free of metrics-library
// imports. Uber Fx binds one instance to all three interfaces.
package bootstrap

import (
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
)

type instrumentation struct {
	m *metrics.Metrics
}

// newInstrumentation is the fx provider for the metric adapter.
func newInstrumentation(m *metrics.Metrics) *instrumentation {
	return &instrumentation{m: m}
}

// --- service.Instrumentation ------------------------------------------------

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

// --- messaging.Instrumentation ----------------------------------------------

func (i *instrumentation) OutboxPublished() { i.m.OutboxPublished.Add(1) }
func (i *instrumentation) OutboxFailed()    { i.m.OutboxFailures.Add(1) }

func (i *instrumentation) OutboxStats(pending int64, lagSeconds float64) {
	i.m.OutboxPending.Set(float64(pending))
	i.m.OutboxLagSeconds.Set(lagSeconds)
}

// --- messaging.ConsumerMetrics ----------------------------------------------

func (i *instrumentation) Received()         { i.m.ConsumerReceived.Add(1) }
func (i *instrumentation) Retry()            { i.m.ConsumerRetries.Add(1) }
func (i *instrumentation) DLQ(reason string) { i.m.ConsumerDLQ.Add(1) }

// Invalid records a structurally invalid payload. Poison bodies are already
// counted by ConsumerDLQ; the parameter stays for the port contract.
func (i *instrumentation) Invalid(reason string) { _ = reason }
