// Package metrics_test pins the metric set as an *observable contract*, not
// just as a struct that constructs.
//
// The distinction matters here: a counter that is built but never registered
// exports nothing, and a metric whose name drifts silently breaks the Grafana
// dashboards and alerts that reference it. So the tests below scrape the
// registry and assert on the real series names — the same strings the README's
// observability section documents — rather than trusting the field names.
package metrics_test

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
)

// gather returns every metric family currently registered.
func gather(t *testing.T, m *metrics.Metrics) []*dto.MetricFamily {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	return families
}

// TestRegistryIsIsolatedPerInstance is the reason the package builds its own
// registry instead of using the global one: two Fx apps (or two tests) in the
// same process must not panic with a duplicate-collector error. We prove it by
// constructing two independent sets.
func TestRegistryIsIsolatedPerInstance(t *testing.T) {
	a, b := metrics.New(), metrics.New()
	assert.NotSame(t, a.Registry(), b.Registry(), "each set owns its registry")

	// Neither registration may panic — the plain act of calling New twice is
	// the assertion.
	require.NotNil(t, a.Registry())
	require.NotNil(t, b.Registry())
}

// TestEveryMetricIsRegisteredAndLabelled checks the two failure modes that
// silently break dashboards:
//   - a collector that was built but never registered (absent from Gather);
//   - a labelled counter that exports no series until first use, which is
//     expected for vectors but must still be present as a family once used.
func TestEveryMetricIsRegisteredAndLabelled(t *testing.T) {
	m := metrics.New()

	// Touch every counter/gauge/histogram so the vectors materialise a series
	// in the registry: an unused vector is registered but gathers nothing, and
	// "registered" is exactly what we want to observe here.
	m.RequestCount.With("endpoint", "/x", "success", "true", "error", "").Add(1)
	m.RequestLatency.With("endpoint", "/x").Observe(0.01)
	m.WagerResults.With("kind", "BET", "status", "PROCESSED", "failure_code", "", "channel", "HTTP").Add(1)
	m.Duplicates.With("source", "inbox").Add(1)
	m.OutboxPublished.Add(1)

	names := map[string]bool{}
	for _, f := range gather(t, m) {
		names[f.GetName()] = true
	}

	// Spot-check the series the README documents. Using exact names here is
	// deliberate: if someone renames a metric, this fails loudly instead of
	// letting a dashboard go blank in production.
	want := []string{
		"wallet_endpoint_requests_total",
		"wallet_endpoint_request_duration_seconds",
		"wallet_wager_results_total",
		"wallet_wager_duplicates_total",
		"wallet_outbox_published_total",
	}
	for _, n := range want {
		assert.Truef(t, names[n], "metric %q must be registered and exported; got %v", n, names)
	}
}

// TestCountersActuallyCount closes the last gap: registration is not
// observation. We add a known amount and read the value back out of the
// registry, so a counter wired to the wrong vector would be caught.
func TestCountersActuallyCount(t *testing.T) {
	m := metrics.New()
	m.OutboxPublished.Add(3)

	var found bool
	for _, f := range gather(t, m) {
		if f.GetName() != "wallet_outbox_published_total" {
			continue
		}
		found = true
		require.NotEmpty(t, f.GetMetric())
		// Counter values are floats; 3 is exact in binary, so equality is safe.
		assert.Equal(t, 3.0, f.GetMetric()[0].GetCounter().GetValue())
	}
	assert.True(t, found, "wallet_outbox_published_total must be exported")
}

// TestNoMetricUsesTheOldPrefix is a cheap regression guard against the
// namespace drifting from "wallet_" (which every dashboard assumes).
func TestNamespaceIsWallet(t *testing.T) {
	m := metrics.New()
	m.RequestCount.With("endpoint", "/x", "success", "true", "error", "").Add(1)
	for _, f := range gather(t, m) {
		assert.Truef(t, strings.HasPrefix(f.GetName(), "wallet_"),
			"metric %q must live under the wallet_ namespace", f.GetName())
	}
}
