// Package auth validates OAuth2/OIDC bearer tokens issued by the external
// IdP (Keycloak) via its JWKS endpoint. The service never issues tokens nor
// stores passwords (out of scope by the challenge).
//
// Authorization model (see ARCHITECTURE.md):
//   - provider clients (provider-a, provider-b) carry a custom claim
//     "providerId"; they may only touch their own transactions;
//   - the internal client (wallet-internal) carries claim "wallet:internal"
//     and is the only identity allowed to operate wallets.
//
// The scope claim is captured too. It is deliberately NOT a gate for
// authentication or authorization: the identity claims above are signed and
// already un-forgeable, so requiring a scope would add a failure mode without
// adding security. The token is *minted with a minimal scope* (see
// deploy/keycloak/realm-wallet.json) and the scope is exposed here for two
// concrete reasons: it lands in structured logs (a token carrying "email" on
// a machine identity is a misconfiguration worth seeing) and integration tests
// assert the narrowing actually happened.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// Principal is the authenticated identity.
type Principal struct {
	ClientID   string   // OAuth2 client (azp claim)
	ProviderID string   // provider this client is bound to (custom claim)
	Internal   bool     // wallet:internal claim — internal service identity
	Scopes     []string // scope claim, split; informational, never a gate (see package doc)
}

func (p Principal) IsProvider() bool { return p.ProviderID != "" }

// HasScope reports whether the token carries the given scope. It exists for
// observability and tests; authorization itself is claim-based on purpose.
func (p Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Authenticator validates tokens against the IdP JWKS and keeps the key set
// fresh, so key rotation is transparent.
//
// The key set is held behind an atomic.Pointer: the background refresher
// publishes a new set with a single atomic Store, and every Validate call
// takes one atomic Load. No lock is shared between the two, and a token is
// always verified against a complete set — never against one being mutated.
//
// Why not jwk.Cache: its Get() caches the *error* of the first failed fetch
// (returning "cached object is not a Set") without hitting the network again
// for the whole minimum-refresh window. Against an IdP that is down at boot —
// precisely the degradation this service is meant to survive — the set would
// never load. See ARCHITECTURE.md ("JWKS resilience").
type Authenticator struct {
	set      atomic.Pointer[jwk.Set]
	issuer   string
	audience string
	jwksURL  string
	log      *slog.Logger
}

const (
	// bootFetchTimeout bounds the best-effort JWKS fetch during construction.
	// A short bound keeps a slow IdP from delaying process startup; on
	// failure the refresher takes over in the background.
	bootFetchTimeout = 5 * time.Second
	// refreshInterval controls transparent key rotation once loaded.
	refreshInterval = 15 * time.Minute
	// retryInterval is the backoff while the IdP is unreachable.
	retryInterval = 2 * time.Second
)

// publish stores a freshly fetched key set. It takes the set by value and
// allocates a new heap cell for it, so the pointer handed to atomic.Store
// never aliases a loop variable that a later iteration would overwrite.
func (a *Authenticator) publish(set jwk.Set) {
	cell := new(jwk.Set)
	*cell = set
	a.set.Store(cell)
}

// NewAuthenticator builds the validator. The initial JWKS fetch is best
// effort: if the IdP is still booting, construction still succeeds and a
// background refresher keeps retrying until the key set loads.
func NewAuthenticator(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Authenticator, error) {
	if log == nil {
		return nil, errors.New("auth: logger is required")
	}
	a := &Authenticator{
		issuer:   cfg.AuthIssuer,
		audience: cfg.AuthAudience,
		jwksURL:  cfg.JWKSURL(),
		log:      log,
	}
	// Publish an empty set up front: Validate treats "no keys" as a clean
	// 401, never as a nil dereference, even before the first fetch lands.
	empty := jwk.NewSet()
	a.set.Store(&empty)

	fetchCtx, cancel := context.WithTimeout(ctx, bootFetchTimeout)
	defer cancel()
	if set, err := jwk.Fetch(fetchCtx, a.jwksURL); err == nil {
		a.publish(set)
		log.Info("JWKS loaded", slog.String("jwks", a.jwksURL))
	} else {
		log.Warn("JWKS not reachable at boot; retrying in background",
			slog.String("jwks", a.jwksURL), slog.String("error", err.Error()))
	}
	// The refresher owns the background refresh: it retries until the set
	// loads, then rotates on an interval. It stops when ctx is cancelled,
	// so it is tied to the process/Fx lifecycle.
	go a.refresh(ctx)
	return a, nil
}

// refresh loads the JWKS, retrying while the IdP is unreachable, then keeps
// the key set fresh on refreshInterval so rotation is transparent.
func (a *Authenticator) refresh(ctx context.Context) {
	warmTries := 0
	for {
		// A bounded per-attempt timeout: a hung IdP must not stall the loop
		// forever, or rotation would silently stop.
		attemptCtx, cancel := context.WithTimeout(ctx, bootFetchTimeout)
		set, err := jwk.Fetch(attemptCtx, a.jwksURL)
		cancel()

		if err == nil {
			a.publish(set)
			a.log.Info("JWKS loaded", slog.String("jwks", a.jwksURL))
			break
		}
		if ctx.Err() != nil {
			return // shutting down
		}
		warmTries++
		if warmTries == 1 || warmTries%10 == 0 {
			a.log.Warn("JWKS fetch failed; retrying",
				slog.String("jwks", a.jwksURL), slog.String("error", err.Error()),
				slog.Int("attempt", warmTries))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryInterval):
		}
	}

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attemptCtx, cancel := context.WithTimeout(ctx, bootFetchTimeout)
			set, err := jwk.Fetch(attemptCtx, a.jwksURL)
			cancel()
			if err != nil {
				// Keep serving the last known good set; a refresh failure
				// must not lock users out.
				a.log.Warn("JWKS refresh failed; keeping previous keys",
					slog.String("jwks", a.jwksURL), slog.String("error", err.Error()))
				continue
			}
			a.publish(set)
			a.log.Info("JWKS rotated", slog.String("jwks", a.jwksURL))
		}
	}
}

var ErrInvalidToken = errors.New("invalid or missing credentials")

// Validate parses and validates the raw bearer token and extracts the
// Principal. Signature, issuer, expiry and (optional) audience are checked.
func (a *Authenticator) Validate(rawToken string) (Principal, error) {
	set := a.set.Load()
	if set == nil || (*set).Len() == 0 {
		return Principal{}, fmt.Errorf("%w: IdP keys unavailable", ErrInvalidToken)
	}
	opts := []jwt.ParseOption{
		jwt.WithKeySet(*set, jws.WithInferAlgorithmFromKey(true)),
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
	scopeClaim, _ := tok.Get("scope")
	scope, _ := scopeClaim.(string)

	p := Principal{ClientID: clientID, ProviderID: providerID, Internal: internal, Scopes: splitScopes(scope)}
	if !p.Internal && !p.IsProvider() {
		return Principal{}, fmt.Errorf("%w: token carries neither providerId nor internal role", ErrInvalidToken)
	}
	return p, nil
}

// splitScopes turns the space-separated OAuth2 scope claim into a slice,
// dropping empty fields so a blank claim yields a nil slice rather than [""].
func splitScopes(scope string) []string {
	if scope == "" {
		return nil
	}
	fields := strings.Fields(scope)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
