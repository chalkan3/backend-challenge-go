// auth_middleware_test.go pins the observability contract of the HTTP auth
// middleware: a rejected request must be counted on
// wallet_auth_failures_total before it is rejected.
//
// The regression it guards is easy to reintroduce. Counting only inside the
// authorize endpoint middleware looks sufficient and passes review, but a
// request with no token never reaches an endpoint — so the counter stayed at
// zero for the most common failure of all, and the dashboard panel that was
// supposed to reveal token abuse showed nothing.
package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
)

// authFailureReasons reads the current reason labels of the auth counter
// straight from the registry, the same way the /metrics handler would.
func authFailureReasons(t *testing.T, m *metrics.Metrics) map[string]float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)

	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "wallet_auth_failures_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			reason := ""
			for _, l := range metric.GetLabel() {
				if l.GetName() == "reason" {
					reason = l.GetValue()
				}
			}
			out[reason] = metric.GetCounter().GetValue()
		}
	}
	return out
}

func TestAuthMiddlewareCountsMissingToken(t *testing.T) {
	m := metrics.New()
	d := Deps{Met: m}

	reached := false
	handler := d.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/wagering/transactions", nil))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, reached, "the downstream handler must not run")

	reasons := authFailureReasons(t, m)
	assert.Equal(t, float64(1), reasons["missing_bearer_token"],
		"a token-less request must be counted, not silently dropped")
}

// TestAuthMiddlewareCountsEveryRejection checks that repeated rejections
// accumulate instead of resetting, so the series keeps its meaning as a rate.
func TestAuthMiddlewareCountsEveryRejection(t *testing.T) {
	m := metrics.New()
	d := Deps{Met: m}
	handler := d.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/wallets", nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	assert.Equal(t, float64(3), authFailureReasons(t, m)["missing_bearer_token"])
}

// TestAuthFailuresSeriesIsRegistered guards the metric name itself: the
// dashboards query wallet_auth_failures_total{reason=...}, so a rename in
// metrics.go must fail here rather than silently empty a panel.
func TestAuthFailuresSeriesIsRegistered(t *testing.T) {
	m := metrics.New()
	handler := Deps{Met: m}.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/wallets", nil))

	families, err := m.Registry().Gather()
	require.NoError(t, err)

	var found *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "wallet_auth_failures_total" {
			found = f
		}
	}
	require.NotNil(t, found, "wallet_auth_failures_total must be exported")
	assert.Contains(t, found.GetHelp(), "Authentication")

	labels := map[string]bool{}
	for _, l := range found.GetMetric()[0].GetLabel() {
		labels[l.GetName()] = true
	}
	assert.True(t, labels["reason"], "the reason label is what the dashboards group by")
}

// Compile-time guard: the registry returned by Metrics must stay a
// prometheus.Gatherer, which is how the test above reads the series.
var _ prometheus.Gatherer = metrics.New().Registry()
