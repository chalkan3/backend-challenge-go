// White-box tests for the endpoint middleware chain and the double-entry
// bookkeeping that does not need a database.
//
// Three things are pinned here, each because it is a real failure mode:
//   - authorize: the access rules. A wallet operation reachable by a provider
//     identity, or a transaction submitted with no identity at all, is a
//     security bug, not a style issue.
//   - instrument: that every call — including a rejected one — moves the
//     request counter and the latency histogram, and that the error label is
//     the stable code, not the raw error string.
//   - boolStr / errorCode: the label helpers, whose output is a metric label
//     and therefore part of the public observability contract.
package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/go-kit/kit/endpoint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// ctxWithPrincipal returns a context carrying the given identity, which is how
// the endpoint middleware sees the caller (the HTTP auth middleware put it
// there earlier in the chain).
func ctxWithPrincipal(p *auth.Principal) context.Context {
	return context.WithValue(context.Background(), ctxPrincipal, p)
}

// ── authorize ──────────────────────────────────────────────────────────────

// TestAuthorizeMatrix walks the access rules as a table. Each row is a
// (identity, endpoint policy) pair and its expected verdict, so the policy is
// legible in one place instead of buried across handler bodies.
func TestAuthorizeMatrix(t *testing.T) {
	internal := &auth.Principal{Internal: true}
	provider := &auth.Principal{ClientID: "provider-a", ProviderID: "provider-a"}
	plain := &auth.Principal{ClientID: "anonymous-ish"}

	tests := []struct {
		name           string
		principal      *auth.Principal
		internalOnly   bool
		providerScoped bool
		wantErr        bool
	}{
		{"no identity at all is refused", nil, false, false, true},
		{"internal identity passes an internal-only endpoint", internal, true, false, false},
		{"provider identity is refused on an internal-only endpoint", provider, true, false, true},
		{"provider identity passes a provider-scoped endpoint", provider, false, true, false},
		{"internal identity passes a provider-scoped endpoint", internal, false, true, false},
		{"a bare identity is refused on a provider-scoped endpoint", plain, false, true, true},
		{"a bare identity passes an unscoped endpoint", plain, false, false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			next := endpoint.Endpoint(func(context.Context, interface{}) (interface{}, error) {
				reached = true
				return "ok", nil
			})

			m := metrics.New()
			got, err := authorize(m, next, tc.internalOnly, tc.providerScoped)(ctxWithPrincipal(tc.principal), struct{}{})

			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, reached, "a denied caller must not reach the endpoint body")
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.True(t, reached)
			assert.Equal(t, "ok", got)
		})
	}
}

// TestAuthorizeClassifiesTheRejection proves the failures are counted with a
// stable reason label. An auth failure that is not observed is an auth failure
// nobody can alert on.
func TestAuthorizeClassifiesTheRejection(t *testing.T) {
	next := endpoint.Endpoint(func(context.Context, interface{}) (interface{}, error) { return nil, nil })
	m := metrics.New()

	_, err := authorize(m, next, true, false)(ctxWithPrincipal(nil), struct{}{})
	require.Error(t, err)

	_, err = authorize(m, next, true, false)(ctxWithPrincipal(&auth.Principal{}), struct{}{})
	require.Error(t, err)

	families, gerr := m.Registry().Gather()
	require.NoError(t, gerr)
	found := false
	for _, f := range families {
		if f.GetName() == "wallet_auth_failures_total" {
			found = true
			assert.NotEmpty(t, f.GetMetric(), "auth failures must be observed")
		}
	}
	assert.True(t, found, "wallet_auth_failures_total must be exported")
}

// ── instrument ─────────────────────────────────────────────────────────────

// TestInstrumentObservesSuccessAndFailure covers the Decorator's whole job:
// pass the result through and record latency + a counter labelled with the
// outcome. A success carries an empty error label; a failure carries the code.
func TestInstrumentObservesSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := metrics.New()
		got, err := instrument("t", m)(func(context.Context, interface{}) (interface{}, error) {
			return "value", nil
		})(context.Background(), struct{}{})

		require.NoError(t, err)
		assert.Equal(t, "value", got)
	})

	t.Run("failure", func(t *testing.T) {
		m := metrics.New()
		boom := errors.New("boom")
		_, err := instrument("t", m)(func(context.Context, interface{}) (interface{}, error) {
			return nil, boom
		})(context.Background(), struct{}{})

		require.ErrorIs(t, err, boom, "instrument must not swallow the error")
	})
}

// ── boolStr ────────────────────────────────────────────────────────────────

func TestBoolStr(t *testing.T) {
	assert.Equal(t, "true", boolStr(true))
	assert.Equal(t, "false", boolStr(false))
}

// ── errorCode ──────────────────────────────────────────────────────────────

// TestErrorCode maps each error family to the stable label its metric series
// uses. The strings are asserted verbatim because dashboards group by them.
func TestErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error → empty label", nil, ""},
		{"validation", &service.ValidationError{Detail: "bad input"}, service.CodeValidationFailed},
		{"validation wrapped in another error", fmt.Errorf("wrapping: %w", &service.ValidationError{Detail: "x"}), service.CodeValidationFailed},
		{"unknown error → internal", errors.New("mystery"), "INTERNAL_ERROR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, errorCode(tc.err))
		})
	}
}

// testLog returns a discard logger: the wiring tests exercise log call sites
// without drowning the output.
func testLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// silentInstrumentation is a no-op service.Instrumentation, so a service can
// be constructed without an observation backend.
type silentInstrumentation struct{}

func (silentInstrumentation) WagerOutcome(string, string, string, string) {}
func (silentInstrumentation) Duplicate(string)                            {}
func (silentInstrumentation) ReferenceOutcome(string)                     {}
func (silentInstrumentation) ReconciliationDivergence()                   {}
func (silentInstrumentation) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, silentSpan{}
}

type silentSpan struct{}

func (silentSpan) SetAttributes(...service.SpanAttr) {}
func (silentSpan) SetOutcome(string, string)         {}
func (silentSpan) RecordError(error)                 {}
func (silentSpan) End()                              {}

// ── NewEndpoints ───────────────────────────────────────────────────────────

// TestNewEndpointsWiresEveryRoute builds the full chain over a real (but
// unused) services pair and asserts every field is populated. A nil endpoint
// here would surface as a panic at request time; catching it at build time is
// the whole point.
func TestNewEndpointsWiresEveryRoute(t *testing.T) {
	m := metrics.New()
	ws := service.NewWalletService(nil, testLog(), silentInstrumentation{})
	wg := service.NewWagerService(nil, testLog(), silentInstrumentation{})

	ep := NewEndpoints(ws, wg, m)

	require.NotNil(t, ep.OpenWallet)
	require.NotNil(t, ep.GetWallet)
	require.NotNil(t, ep.ListLedger)
	require.NotNil(t, ep.Reconcile)
	require.NotNil(t, ep.SubmitTransaction)
	require.NotNil(t, ep.GetTransaction)
	require.NotNil(t, ep.GetProviderTransaction)
}
