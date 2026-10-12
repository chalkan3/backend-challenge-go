// Package auth validates OAuth2/OIDC bearer tokens issued by the external
// IdP (Keycloak) via its JWKS endpoint. The service never issues tokens nor
// stores passwords (out of scope by the challenge).
//
// Authorization model (see ARCHITECTURE.md):
//   - provider clients (provider-a, provider-b) carry a custom claim
//     "providerId"; they may only touch their own transactions;
//   - the internal client (wallet-internal) carries claim "wallet:internal"
//     and is the only identity allowed to operate wallets.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// Principal is the authenticated identity.
type Principal struct {
	ClientID   string // OAuth2 client (azp claim)
	ProviderID string // provider this client is bound to (custom claim)
	Internal   bool   // wallet:internal claim — internal service identity
}

func (p Principal) IsProvider() bool { return p.ProviderID != "" }

// Authenticator validates tokens against the IdP JWKS with automatic cache
// refresh, so key rotation is transparent.
type Authenticator struct {
	set      jwk.Set
	issuer   string
	audience string
	log      *slog.Logger
}

// NewAuthenticator builds the validator. The initial JWKS fetch is best
// effort (the IdP may still be booting); the cache refreshes lazily.
func NewAuthenticator(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Authenticator, error) {
	cache := jwk.NewCache(ctx)
	if err := cache.Register(cfg.JWKSURL(), jwk.WithMinRefreshInterval(15*time.Minute)); err != nil {
		return nil, err
	}
	a := &Authenticator{issuer: cfg.AuthIssuer, audience: cfg.AuthAudience, log: log}
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	set, err := cache.Get(fetchCtx, cfg.JWKSURL())
	if err != nil {
		log.Warn("JWKS not reachable at boot; will retry lazily on first request",
			slog.String("jwks", cfg.JWKSURL()), slog.String("error", err.Error()))
		a.set = jwk.NewSet()
		go func() {
			// Background warm-up with a generous window.
			warmCtx, warmCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer warmCancel()
			for {
				s, err := cache.Get(warmCtx, cfg.JWKSURL())
				if err == nil {
					a.set = s
					log.Info("JWKS loaded", slog.String("jwks", cfg.JWKSURL()))
					return
				}
				if warmCtx.Err() != nil {
					return
				}
				time.Sleep(2 * time.Second)
			}
		}()
	} else {
		a.set = set
	}
	return a, nil
}

var ErrInvalidToken = errors.New("invalid or missing credentials")

// Validate parses and validates the raw bearer token and extracts the
// Principal. Signature, issuer, expiry and (optional) audience are checked.
func (a *Authenticator) Validate(rawToken string) (Principal, error) {
	if a.set.Len() == 0 {
		return Principal{}, fmt.Errorf("%w: IdP keys unavailable", ErrInvalidToken)
	}
	opts := []jwt.ParseOption{
		jwt.WithKeySet(a.set, jws.WithInferAlgorithmFromKey(true)),
		jwt.WithIssuer(a.issuer),
	}
	if a.audience != "" {
		opts = append(opts, jwt.WithAudience(a.audience))
	}
	tok, err := jwt.Parse([]byte(rawToken), opts...)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	azp, _ := tok.Get("azp")
	clientID, _ := azp.(string)
	providerClaim, _ := tok.Get("providerId")
	providerID, _ := providerClaim.(string)
	internalClaim, _ := tok.Get("wallet:internal")
	internal, _ := internalClaim.(bool)

	p := Principal{ClientID: clientID, ProviderID: providerID, Internal: internal}
	if !p.Internal && !p.IsProvider() {
		return Principal{}, fmt.Errorf("%w: token carries neither providerId nor internal role", ErrInvalidToken)
	}
	return p, nil
}
