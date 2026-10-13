//go:build integration

// The router is assembled by bootstrap in production; the integration suite
// drives the composed app over the network (so its coverage lands in the app
// container, not this binary). This test builds the router in-process with the
// real dependencies and a real Keycloak-shaped JWKS, so the route table, the
// auth middleware, the access log, the readiness probe and the tracing
// wrapper are exercised where coverage is recorded.
package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// --- router harness ---------------------------------------------------------

func routerSQS(t *testing.T) *awssqs.Client {
	t.Helper()
	endpoint := os.Getenv("WALLET_TEST_SQS_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)
	return awssqs.NewFromConfig(cfg, func(o *awssqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

func newRouterDeps(t *testing.T, mode config.Mode) (Deps, *routerIDP) {
	t.Helper()
	store := postgres.NewStore(endpointPool(t), nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hook := silentHook{}
	ws := service.NewWalletService(store, log, hook)
	wg := service.NewWagerService(store, log, hook)
	m := metrics.New()
	idp := newRouterIDP(t)

	cfg := &config.Config{
		Mode:            mode,
		HTTPAddr:        ":0",
		LogLevel:        "info",
		SQSRequestQueue: "wager-transactions.fifo",
	}
	d := Deps{
		Cfg:  cfg,
		Log:  log,
		Auth: idp.auth,
		Ep:   NewEndpoints(ws, wg, m),
		Pool: endpointPool(t),
		SQS:  routerSQS(t),
		URLs: sqsinfra.QueueURLs{},
		Reg:  m.Registry(),
		OTel: nil,
		Met:  m,
	}
	return d, idp
}

func do(t *testing.T, h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- tests ------------------------------------------------------------------

func TestRouterServesPublicRoutesInEveryMode(t *testing.T) {
	d, _ := newRouterDeps(t, config.ModeAll)
	r := NewRouter(d)

	live := do(t, r, "GET", "/health/live", "")
	assert.Equal(t, http.StatusOK, live.Code)
	assert.JSONEq(t, `{"status":"live"}`, live.Body.String())

	// /metrics must be scrapable without identity.
	metricsResp := do(t, r, "GET", "/metrics", "")
	assert.Equal(t, http.StatusOK, metricsResp.Code)
}

func TestRouterReadyProbesDependencies(t *testing.T) {
	d, _ := newRouterDeps(t, config.ModeAll)
	r := NewRouter(d)

	// Postgres and SQS are up in the integration environment → ready.
	rec := do(t, r, "GET", "/health/ready", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ready"}`, rec.Body.String())
}

func TestRouterBusinessRoutesRequireIdentity(t *testing.T) {
	d, idp := newRouterDeps(t, config.ModeAll)
	r := NewRouter(d)

	// No bearer token → 401 from the HTTP auth middleware.
	rec := do(t, r, "GET", "/wallets/"+uuid.NewString(), "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Invalid bearer token → 401.
	rec = do(t, r, "GET", "/wallets/"+uuid.NewString(), "garbage")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Valid internal token → the endpoint runs; unknown wallet → 404.
	internal := idp.token(t, map[string]any{"azp": "wallet-internal", "wallet:internal": true})
	rec = do(t, r, "GET", "/wallets/"+uuid.NewString(), internal)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Authenticated, but not authorized for wallet routes → 403.
	provider := idp.token(t, map[string]any{"azp": "provider-a", "providerId": "provider-a"})
	rec = do(t, r, "GET", "/wallets/"+uuid.NewString(), provider)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestRouterUnknownRouteIsNotFound(t *testing.T) {
	d, _ := newRouterDeps(t, config.ModeAll)
	r := NewRouter(d)

	rec := do(t, r, "GET", "/no/such/route", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_FOUND")
}

func TestRouterWorkersModeSkipsBusinessRoutes(t *testing.T) {
	d, idp := newRouterDeps(t, config.ModeWorkers)
	r := NewRouter(d)

	// Health still served (probes must not depend on the role).
	rec := do(t, r, "GET", "/health/live", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	// Business route is not even mounted → 404 even WITH a valid token.
	internal := idp.token(t, map[string]any{"azp": "wallet-internal", "wallet:internal": true})
	rec = do(t, r, "GET", "/wallets/"+uuid.NewString(), internal)
	assert.Equal(t, http.StatusNotFound, rec.Code, "a worker must not take business traffic")
}

func TestNewServerPopulatesTimeouts(t *testing.T) {
	d, _ := newRouterDeps(t, config.ModeAll)
	srv := NewServer(d)
	require.NotNil(t, srv)
	assert.Equal(t, ":0", srv.Addr)
	assert.NotNil(t, srv.Handler)
	assert.NotZero(t, srv.ReadHeaderTimeout)
	assert.NotZero(t, srv.ReadTimeout)
	assert.NotZero(t, srv.WriteTimeout)
	assert.NotZero(t, srv.IdleTimeout)
}

func TestBearerTokenParsing(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/", nil)
	assert.Empty(t, bearerToken(req))

	req.Header.Set("Authorization", "Bearer abc")
	assert.Equal(t, "abc", bearerToken(req))

	req.Header.Set("Authorization", "Basic abc")
	assert.Empty(t, bearerToken(req))

	// "Bearer " with nothing after it is not a token.
	req.Header.Set("Authorization", "Bearer ")
	assert.Empty(t, bearerToken(req))
}

func TestStatusWriterRecordsCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	sw.WriteHeader(http.StatusTeapot)
	assert.Equal(t, http.StatusTeapot, sw.status)
}
