// Package auth validates OIDC access tokens against the realm JWKS. These
// tests spin a real JWKS endpoint backed by a freshly generated RSA key so
// the production validation path (signature, iss, exp, claims) is exercised,
// including the expired-credential case required by challenge §13.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// testIdP bundles an in-memory identity provider with the authenticator
// wired to it and the signing key used to mint tokens.
type testIdP struct {
	auth   *Authenticator
	key    jwk.Key
	issuer string
}

func newTestIdP(t *testing.T) *testIdP {
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	issuer := srv.URL + "/realms/test"
	cfg := &config.Config{AuthIssuer: issuer, AuthJWKSURL: srv.URL + "/certs"}
	a, err := NewAuthenticator(context.Background(), cfg, slog.Default())
	require.NoError(t, err)

	signing, err := jwk.FromRaw(priv)
	require.NoError(t, err)
	require.NoError(t, signing.Set(jwk.KeyIDKey, "test-key-1"))
	require.NoError(t, signing.Set(jwk.AlgorithmKey, jwa.RS256))

	return &testIdP{auth: a, key: signing, issuer: issuer}
}

func (p *testIdP) sign(t *testing.T, claims map[string]any, exp time.Time) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(p.issuer).
		IssuedAt(time.Now().Add(-time.Minute)).
		Expiration(exp)
	for k, v := range claims {
		b.Claim(k, v)
	}
	tok, err := b.Build()
	require.NoError(t, err)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, p.key))
	require.NoError(t, err)
	return string(signed)
}

func providerClaims() map[string]any {
	return map[string]any{"azp": "provider-a", "providerId": "provider-a"}
}

func internalClaims() map[string]any {
	return map[string]any{"azp": "wallet-internal", "wallet:internal": true}
}

func TestAuthenticatorAcceptsValidProviderToken(t *testing.T) {
	idp := newTestIdP(t)
	raw := idp.sign(t, providerClaims(), time.Now().Add(10*time.Minute))

	p, err := idp.auth.Validate(raw)
	require.NoError(t, err)
	assert.Equal(t, "provider-a", p.ProviderID)
	assert.False(t, p.Internal)
	assert.True(t, p.IsProvider())
}

func TestAuthenticatorAcceptsValidInternalToken(t *testing.T) {
	idp := newTestIdP(t)
	raw := idp.sign(t, internalClaims(), time.Now().Add(10*time.Minute))

	p, err := idp.auth.Validate(raw)
	require.NoError(t, err)
	assert.True(t, p.Internal)
	assert.False(t, p.IsProvider(), "internal identity is not bound to a provider")
}

func TestAuthenticatorRejections(t *testing.T) {
	tests := []struct {
		name   string
		claims func(idp *testIdP) string
	}{
		{
			name: "expired token",
			claims: func(idp *testIdP) string {
				return idp.sign(t, providerClaims(), time.Now().Add(-10*time.Minute))
			},
		},
		{
			name: "wrong issuer",
			claims: func(idp *testIdP) string {
				rogue := *idp
				rogue.issuer = "http://rogue-realm/realms/other"
				return rogue.sign(t, providerClaims(), time.Now().Add(10*time.Minute))
			},
		},
		{
			name: "unprivileged token (no provider, no internal role)",
			claims: func(idp *testIdP) string {
				return idp.sign(t, map[string]any{"azp": "some-other-service"}, time.Now().Add(10*time.Minute))
			},
		},
		{
			name:   "garbage token",
			claims: func(*testIdP) string { return "not-a-jwt" },
		},
		{
			name:   "empty token",
			claims: func(*testIdP) string { return "" },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idp := newTestIdP(t)
			raw := tc.claims(idp)
			_, err := idp.auth.Validate(raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidToken)
		})
	}
}

func TestAuthenticatorRejectsUnknownSigningKey(t *testing.T) {
	idp := newTestIdP(t)

	// A structurally valid token signed by a key that is NOT in the JWKS.
	roguePriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rogueKey, err := jwk.FromRaw(roguePriv)
	require.NoError(t, err)
	require.NoError(t, rogueKey.Set(jwk.KeyIDKey, "rogue-key"))
	require.NoError(t, rogueKey.Set(jwk.AlgorithmKey, jwa.RS256))

	rogue := &testIdP{auth: idp.auth, key: rogueKey, issuer: idp.issuer}
	raw := rogue.sign(t, providerClaims(), time.Now().Add(10*time.Minute))

	_, err = idp.auth.Validate(raw)
	require.ErrorIs(t, err, ErrInvalidToken,
		"a token signed by an unknown key must never authenticate")
}
