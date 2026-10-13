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

	"github.com/chalkan3/backend-challenge-go/internal/auth"
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

// TestAuthMiddlewareInjectsIdentityOnValidToken covers the branch every
// authenticated request depends on: a token the authenticator accepts must
// produce a principal in the context, an Idempotency-Key, and a correlation id
// that is either honoured from the header or generated. The rejection branches
// are pinned above; without this one the success path would be exercised only
// inside the app container, where the unit coverage never reaches it.
func TestAuthMiddlewareInjectsIdentityOnValidToken(t *testing.T) {
	idp := newRouterIDP(t)
	m := metrics.New()
	d := Deps{Met: m, Auth: idp.auth}

	var gotPrincipal *auth.Principal
	var gotKey, gotCorrelation string
	handler := d.auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPrincipal = principalFrom(r.Context())
		gotKey = idempotencyKeyFrom(r.Context())
		gotCorrelation = correlationFrom(r.Context())
	}))

	token := idp.token(t, map[string]any{"azp": "provider-a", "providerId": "provider-a"})

	t.Run("correlation id is taken from the header", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/wagering/transactions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "idem-abc")
		req.Header.Set("X-Correlation-Id", "corr-xyz")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code, "a valid token must reach the handler")
		require.NotNil(t, gotPrincipal, "the principal must be in the context")
		assert.Equal(t, "provider-a", gotPrincipal.ProviderID)
		assert.Equal(t, "idem-abc", gotKey)
		assert.Equal(t, "corr-xyz", gotCorrelation)
		assert.Equal(t, "corr-xyz", rec.Header().Get("X-Correlation-Id"),
			"the correlation id is echoed so the caller can join its trace")
	})

	t.Run("a missing correlation id is generated", func(t *testing.T) {
		gotCorrelation = ""
		req := httptest.NewRequest("POST", "/wagering/transactions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.NotEmpty(t, gotCorrelation, "every request gets a correlation id, given or made")
		assert.Equal(t, gotCorrelation, rec.Header().Get("X-Correlation-Id"))
	})

	assert.Empty(t, authFailureReasons(t, m), "a valid token must not count as a failure")
}
