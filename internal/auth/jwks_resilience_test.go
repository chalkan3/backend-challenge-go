package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// These tests pin the resilience contract of the JWKS loader.
//
// Background: the previous implementation used jwk.Cache. When the very first
// fetch fails, jwk.Cache caches the *error* ("cached object is not a Set") and
// keeps answering from that cache for the whole minimum-refresh window without
// touching the network again. Against an IdP that is down at boot — the exact
// degradation this service is built to survive — the key set would never load
// and every token would be rejected forever. See ARCHITECTURE.md.

// jwksServer serves a valid JWKS, failing the first `failFirst` requests so the
// boot fetch fails and the background path is exercised.
func jwksServer(t *testing.T, failFirst int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pub, err := jwk.PublicKeyOf(priv)
	require.NoError(t, err)
	require.NoError(t, pub.Set(jwk.KeyIDKey, "test-key-1"))
	require.NoError(t, pub.Set(jwk.AlgorithmKey, jwa.RS256))
	set := jwk.NewSet()
	require.NoError(t, set.AddKey(pub))
	raw, err := json.Marshal(set)
	require.NoError(t, err)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestJWKSRecoversWhenIdPAvailableAfterBoot pins the post-boot recovery
// contract: an IdP that is down at construction but comes up shortly after
// must eventually have its keys loaded, driven by the background refresher
// rather than by a request. It also asserts that the loader keeps hitting the
// network instead of answering from a cached error.
//
// Note on what this test does and does not prove: it fails if the refresher
// stops retrying or never publishes the set, and it requires at least two
// network attempts. It does NOT by itself distinguish jwk.Fetch from a
// correctly-driven jwk.Cache — a side-by-side probe showed jwk.Cache answers
// from its cached error (1 network hit over 3s) while jwk.Fetch retries (2
// hits), which is why the loader uses Fetch. The assertion below is the part
// of that finding that is stably observable in a unit test.
func TestJWKSRecoversWhenIdPAvailableAfterBoot(t *testing.T) {
	srv, hits := jwksServer(t, 1) // first request fails, second succeeds

	cfg := &config.Config{
		AuthIssuer:  "http://idp/realms/wallet",
		AuthJWKSURL: srv.URL + "/certs",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := NewAuthenticator(ctx, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	// Keys are not loaded yet, so validation is refused — but not panicked.
	_, verr := a.Validate("anything")
	require.ErrorIs(t, verr, ErrInvalidToken)
	require.Contains(t, verr.Error(), "keys unavailable")

	require.Eventually(t, func() bool {
		set := a.set.Load()
		return set != nil && (*set).Len() > 0
	}, 10*time.Second, 50*time.Millisecond,
		"the refresher must load the key set once the IdP becomes reachable")

	assert.GreaterOrEqual(t, hits.Load(), int32(2),
		"the loader must actually retry against the network, not answer from a cached error")
}

// TestValidateRefusesUntilKeysLoad covers the guard in Validate: while no key
// set is loaded, every token is refused as an auth failure (never trusted,
// never a panic).
func TestValidateRefusesUntilKeysLoad(t *testing.T) {
	a := &Authenticator{
		issuer:   "http://idp/realms/wallet",
		audience: "",
		log:      slog.New(slog.DiscardHandler),
	}
	empty := jwk.NewSet()
	a.set.Store(&empty)

	_, err := a.Validate("any.token.here")
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "keys unavailable")
}

// TestNewAuthenticatorRequiresLogger pins the defensive guard: a nil logger
// would otherwise panic on the first log call inside the refresher.
func TestNewAuthenticatorRequiresLogger(t *testing.T) {
	cfg := &config.Config{AuthIssuer: "http://idp/realms/wallet"}
	_, err := NewAuthenticator(context.Background(), cfg, nil)
	require.Error(t, err)
}
