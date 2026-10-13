package auth

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// These tests cover the two paths the happy-path harness never reaches: the
// lazy warm-up when the IdP is unreachable at boot, and the defensive
// branches inside Validate (no keys loaded yet, optional audience check).

// TestValidateWithNoKeysLoadedIsRejected covers the "IdP keys unavailable"
// guard: before the first successful JWKS fetch there is nothing to verify a
// signature against, so every token must be refused rather than trusted.
func TestValidateWithNoKeysLoadedIsRejected(t *testing.T) {
	t.Parallel()
	a := &Authenticator{
		issuer:   "http://idp/realms/wallet",
		audience: "",
		log:      slog.New(slog.DiscardHandler),
	}
	// Store an empty set: registered but never fetched successfully.
	empty := jwk.NewSet()
	a.set.Store(&empty)

	_, err := a.Validate("any.token.here")
	require.ErrorIs(t, err, ErrInvalidToken)
	assert.Contains(t, err.Error(), "keys unavailable")
}

// TestNewAuthenticatorSurvivesUnreachableIdP covers the boot best-effort path:
// with the IdP still starting, the constructor must succeed (and schedule the
// lazy warm-up) instead of failing the whole process.
func TestNewAuthenticatorSurvivesUnreachableIdP(t *testing.T) {
	// An address that refuses connections, so the initial fetch fails fast.
	cfg := &config.Config{
		AuthIssuer:  "http://127.0.0.1:1/realms/wallet",
		AuthJWKSURL: "http://127.0.0.1:1/certs",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the warm-up goroutine

	a, err := NewAuthenticator(ctx, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err, "an unreachable IdP must not fail construction")
	require.NotNil(t, a)

	// No keys were loaded, so validation is refused (not panicked).
	_, verr := a.Validate("whatever")
	require.ErrorIs(t, verr, ErrInvalidToken)
}

// TestValidateEnforcesAudienceWhenConfigured covers the optional audience
// option: a token whose audience does not match must be refused, while one
// that does must pass.
func TestValidateEnforcesAudienceWhenConfigured(t *testing.T) {
	idp := newTestIdP(t)
	// Re-wire the authenticator with the same key set but an audience check.
	withAud := &Authenticator{
		issuer:   idp.auth.issuer,
		audience: "wallet-api",
		log:      slog.New(slog.DiscardHandler),
	}
	// Publish the same key set via the atomic accessor, not by copying it.
	withAud.publish(*idp.auth.set.Load())

	// Token WITHOUT the required audience → rejected.
	noAud := idp.sign(t, providerClaims(), time.Now().Add(10*time.Minute))
	_, err := withAud.Validate(noAud)
	require.ErrorIs(t, err, ErrInvalidToken)

	// Token WITH the required audience → accepted.
	claims := providerClaims()
	claims["aud"] = "wallet-api"
	withAud2 := &Authenticator{
		issuer:   idp.auth.issuer,
		audience: "wallet-api",
		log:      slog.New(slog.DiscardHandler),
	}
	withAud2.publish(*idp.auth.set.Load())
	ok := idp.sign(t, claims, time.Now().Add(10*time.Minute))
	p, err := withAud2.Validate(ok)
	require.NoError(t, err)
	assert.Equal(t, "provider-a", p.ProviderID)
}

// TestValidateCapturesScopeButDoesNotGateOnIt pins the scope policy: the scope
// claim is parsed into Principal.Scopes (so it can be logged and asserted),
// while carrying the identity claims alone is still sufficient to authenticate.
// Minting a minimal scope is a realm concern; refusing a token because of a
// missing scope is NOT — see the package doc.
func TestValidateCapturesScopeButDoesNotGateOnIt(t *testing.T) {
	t.Parallel()
	idp := newTestIdP(t)

	// A provider token whose scope is the narrowed wallet scope.
	claims := providerClaims()
	claims["scope"] = "wallet:provider wallet:audience"
	raw := idp.sign(t, claims, time.Now().Add(10*time.Minute))
	p, err := idp.auth.Validate(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"wallet:provider", "wallet:audience"}, p.Scopes)
	assert.True(t, p.HasScope("wallet:provider"))
	assert.False(t, p.HasScope("wallet:internal"))

	// The same identity with a wide scope still authenticates: scope is not a
	// gate. A token with NO scope claim at all is equally accepted.
	wide := providerClaims()
	wide["scope"] = "profile email"
	pw, err := idp.auth.Validate(idp.sign(t, wide, time.Now().Add(10*time.Minute)))
	require.NoError(t, err)
	assert.False(t, pw.HasScope("wallet:provider"), "wide scope must not fake the wallet scope")

	none := idp.sign(t, providerClaims(), time.Now().Add(10*time.Minute))
	pn, err := idp.auth.Validate(none)
	require.NoError(t, err)
	assert.Nil(t, pn.Scopes, "absent scope claim yields nil, not [\"\"]")
}
