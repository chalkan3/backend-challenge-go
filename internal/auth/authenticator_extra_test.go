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
		set:      jwk.NewSet(), // registered but never fetched
		issuer:   "http://idp/realms/wallet",
		audience: "",
		log:      slog.New(slog.DiscardHandler),
	}

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
		set:      idp.auth.set,
		issuer:   idp.auth.issuer,
		audience: "wallet-api",
		log:      slog.New(slog.DiscardHandler),
	}

	// Token WITHOUT the required audience → rejected.
	noAud := idp.sign(t, providerClaims(), time.Now().Add(10*time.Minute))
	_, err := withAud.Validate(noAud)
	require.ErrorIs(t, err, ErrInvalidToken)

	// Token WITH the required audience → accepted.
	claims := providerClaims()
	claims["aud"] = "wallet-api"
	withAud2 := &Authenticator{
		set:      idp.auth.set,
		issuer:   idp.auth.issuer,
		audience: "wallet-api",
		log:      slog.New(slog.DiscardHandler),
	}
	ok := idp.sign(t, claims, time.Now().Add(10*time.Minute))
	p, err := withAud2.Validate(ok)
	require.NoError(t, err)
	assert.Equal(t, "provider-a", p.ProviderID)
}
