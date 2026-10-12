package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// newTestIdP spins an httptest JWKS endpoint backed by a freshly generated
// RSA key and returns an authenticator wired to it, plus the signing key.
// This exercises the REAL validation path (signature, iss, exp, claims) and
// makes the EXPIRED-token case testable without waiting for the live
// Keycloak access-token lifespan (challenge §13: missing, invalid AND
// expired credentials must be rejected).
func newTestIdP(t *testing.T) (*Authenticator, jwk.Key, string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := jwk.PublicKeyOf(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Set(jwk.KeyIDKey, "test-key-1"); err != nil {
		t.Fatal(err)
	}
	if err := pub.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatal(err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	issuer := srv.URL + "/realms/test"
	cfg := &config.Config{AuthIssuer: issuer, AuthJWKSURL: srv.URL + "/certs"}
	a, err := NewAuthenticator(context.Background(), cfg, slog.Default())
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}

	signing, err := jwk.FromRaw(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.Set(jwk.KeyIDKey, "test-key-1"); err != nil {
		t.Fatal(err)
	}
	if err := signing.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatal(err)
	}
	return a, signing, issuer
}

func signToken(t *testing.T, key jwk.Key, issuer string, claims map[string]any, exp time.Time) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(issuer).
		IssuedAt(time.Now().Add(-time.Minute)).
		Expiration(exp)
	for k, v := range claims {
		b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, key))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func TestAuthenticatorAcceptsValidProviderToken(t *testing.T) {
	a, key, issuer := newTestIdP(t)
	raw := signToken(t, key, issuer, map[string]any{"azp": "provider-a", "providerId": "provider-a"},
		time.Now().Add(10*time.Minute))
	p, err := a.Validate(raw)
	if err != nil {
		t.Fatalf("valid provider token rejected: %v", err)
	}
	if p.ClientID != "provider-a" || p.ProviderID != "provider-a" || p.Internal {
		t.Fatalf("principal mismatch: %+v", p)
	}
}

func TestAuthenticatorAcceptsInternalToken(t *testing.T) {
	a, key, issuer := newTestIdP(t)
	raw := signToken(t, key, issuer, map[string]any{"azp": "wallet-internal", "wallet:internal": true},
		time.Now().Add(10*time.Minute))
	p, err := a.Validate(raw)
	if err != nil {
		t.Fatalf("valid internal token rejected: %v", err)
	}
	if !p.Internal || p.IsProvider() {
		t.Fatalf("principal mismatch: %+v", p)
	}
}

func TestAuthenticatorRejectsExpiredToken(t *testing.T) {
	a, key, issuer := newTestIdP(t)
	raw := signToken(t, key, issuer, map[string]any{"azp": "provider-a", "providerId": "provider-a"},
		time.Now().Add(-10*time.Minute)) // expired 10 minutes ago
	if _, err := a.Validate(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token must be rejected with ErrInvalidToken, got: %v", err)
	}
}

func TestAuthenticatorRejectsWrongIssuer(t *testing.T) {
	a, key, _ := newTestIdP(t)
	raw := signToken(t, key, "https://evil.example/realms/test",
		map[string]any{"azp": "provider-a", "providerId": "provider-a"}, time.Now().Add(10*time.Minute))
	if _, err := a.Validate(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("foreign-issuer token must be rejected, got: %v", err)
	}
}

func TestAuthenticatorRejectsForeignSignature(t *testing.T) {
	a, _, issuer := newTestIdP(t)
	rogue, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rogueKey, err := jwk.FromRaw(rogue)
	if err != nil {
		t.Fatal(err)
	}
	if err := rogueKey.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatal(err)
	}
	raw := signToken(t, rogueKey, issuer,
		map[string]any{"azp": "provider-a", "providerId": "provider-a"}, time.Now().Add(10*time.Minute))
	if _, err := a.Validate(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token signed by an unknown key must be rejected, got: %v", err)
	}
}

func TestAuthenticatorRejectsUnprivilegedToken(t *testing.T) {
	a, key, issuer := newTestIdP(t)
	// signed by the trusted IdP but carrying neither providerId nor the
	// internal role: it must not unlock anything.
	raw := signToken(t, key, issuer, map[string]any{"azp": "some-other-service"}, time.Now().Add(10*time.Minute))
	if _, err := a.Validate(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unprivileged token must be rejected, got: %v", err)
	}
}
