// A minimal Keycloak-shaped identity provider for the HTTP tests: an RSA key,
// a JWKS endpoint serving its public half, and a builder for signed tokens.
//
// It lives outside the integration build tag because the auth middleware's
// success path — the branch that injects the principal and the correlation id
// into the request context — can only run with a token the authenticator
// accepts. Without a real signature that branch would be permanently dark in
// the unit build, which is exactly the branch every authenticated request
// depends on.
package http

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
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

type routerIDP struct {
	auth   *auth.Authenticator
	key    jwk.Key
	issuer string
}

func newRouterIDP(t *testing.T) *routerIDP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pub, err := jwk.PublicKeyOf(priv)
	require.NoError(t, err)
	require.NoError(t, pub.Set(jwk.KeyIDKey, "router-test-key"))
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

	issuer := srv.URL + "/realms/wallet"
	cfg := &config.Config{AuthIssuer: issuer, AuthJWKSURL: srv.URL + "/certs"}
	a, err := auth.NewAuthenticator(context.Background(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	signing, err := jwk.FromRaw(priv)
	require.NoError(t, err)
	require.NoError(t, signing.Set(jwk.KeyIDKey, "router-test-key"))
	require.NoError(t, signing.Set(jwk.AlgorithmKey, jwa.RS256))

	return &routerIDP{auth: a, key: signing, issuer: issuer}
}

func (i *routerIDP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(i.issuer).
		IssuedAt(time.Now().Add(-time.Minute)).
		Expiration(time.Now().Add(10 * time.Minute))
	for k, v := range claims {
		b.Claim(k, v)
	}
	tok, err := b.Build()
	require.NoError(t, err)
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, i.key))
	require.NoError(t, err)
	return string(signed)
}
