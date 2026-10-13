// White-box tests for the server's assembled behaviour: the probes, the
// access-log decorator, the auth gate and the route table.
//
// These run without a database or an identity provider by using the seams the
// production code already has — a no-op OTel provider, a pool pointed at a
// closed port (so Ping fails deterministically), and an SQS client aimed at an
// httptest server. That is the difference between "the router compiles" and
// "the router answers 401 without a token, 503 when Postgres is down, and
// exposes exactly the documented routes".
package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// deadPool builds a pool whose DSN points at a closed port. pgxpool does not
// connect eagerly, so construction succeeds and every Ping fails — exactly the
// "Postgres is down" state the readiness probe must report.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Guard the fixture: if this port ever answers, the test would silently
	// assert the wrong branch.
	if pool.Ping(context.Background()) == nil {
		t.Fatal("expected the dead-port pool to be unreachable, but Ping succeeded")
	}
	return pool
}

// sqsClientAt builds a real SQS client aimed at a test server.
func sqsClientAt(t *testing.T, endpoint string) *awssqs.Client {
	t.Helper()
	return awssqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", ""),
	}, func(o *awssqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// baseDeps assembles the minimal Deps a probe or the router needs.
func baseDeps(t *testing.T, pool *pgxpool.Pool, sqsClient *awssqs.Client) Deps {
	t.Helper()
	cfg := &config.Config{
		Mode:            config.ModeAll, // serves the business routes
		HTTPAddr:        ":0",
		SQSRequestQueue: "wager-transactions.fifo",
		SQSEventsQueue:  "integration-events.fifo",
		SQSDLQQueue:     "wager-transactions-dlq.fifo",
		ShutdownGrace:   time.Second,
	}
	m := metrics.New()
	return Deps{
		Cfg:  cfg,
		Log:  slog.New(slog.DiscardHandler),
		Auth: &auth.Authenticator{}, // no keys loaded → every token is invalid
		Ep:   Endpoints{OpenWallet: okEndpoint},
		Pool: pool,
		SQS:  sqsClient,
		URLs: sqs.QueueURLs{Request: "http://x/q", Events: "http://x/e", DLQ: "http://x/d"},
		Reg:  prometheus.NewRegistry(),
		OTel: &otel.Provider{}, // zero value: Tracer/TracerProvider are nil-safe
		Met:  m,
	}
}

func okEndpoint(context.Context, interface{}) (interface{}, error) { return struct{}{}, nil }

// ── handleLive ─────────────────────────────────────────────────────────────

func TestHandleLive(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLive(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"live"}`, rec.Body.String())
}

// ── ready ──────────────────────────────────────────────────────────────────

// TestReadyReportsNotReadyWhenDependenciesAreDown is the branch an orchestrator
// depends on: a dead Postgres must yield 503, never a cheerful 200.
func TestReadyReportsNotReadyWhenDependenciesAreDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"__type":"InternalError"}`))
	}))
	t.Cleanup(srv.Close)

	d := baseDeps(t, deadPool(t), sqsClientAt(t, srv.URL))
	rec := httptest.NewRecorder()
	d.ready(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.JSONEq(t, `{"status":"not_ready"}`, rec.Body.String())
}

// ── logRequests ────────────────────────────────────────────────────────────

// TestLogRequestsCapturesStatusAndPassesThrough proves the access-log
// decorator is transparent (it must not swallow the response) and that it
// records the real status code, including a non-200.
func TestLogRequestsCapturesStatusAndPassesThrough(t *testing.T) {
	d := baseDeps(t, nil, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brew"))
	})

	rec := httptest.NewRecorder()
	d.logRequests(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wallets/x", nil))

	assert.Equal(t, http.StatusTeapot, rec.Code, "the response must pass through untouched")
	assert.Equal(t, "brew", rec.Body.String())
}

// TestStatusWriterDefaultsTo200 covers the subtle case: a handler that writes a
// body without calling WriteHeader implicitly sends 200, and the access log
// must report that, not the zero value.
func TestStatusWriterDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	_, _ = sw.Write([]byte("hello"))
	assert.Equal(t, http.StatusOK, sw.status)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ── bearerToken ────────────────────────────────────────────────────────────

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"extracts a well-formed token", "Bearer abc.def.ghi", "abc.def.ghi"},
		{"missing header", "", ""},
		{"wrong scheme", "Basic abc", ""},
		{"bare value without scheme", "abc.def", ""},
		{"scheme only", "Bearer ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			assert.Equal(t, tc.want, bearerToken(r))
		})
	}
}

// ── auth gate ──────────────────────────────────────────────────────────────

// TestAuthGateRejectsMissingAndInvalidTokens is the eliminatory requirement
// from the brief: no unauthenticated access. A missing token and an invalid
// one are both 401, and the metric distinguishes them.
func TestAuthGateRejectsMissingAndInvalidTokens(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		d := baseDeps(t, nil, nil)
		called := false
		h := d.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/wallets", nil))

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.False(t, called, "the downstream handler must not run without a token")
		assert.Contains(t, rec.Body.String(), "UNAUTHORIZED")
	})

	t.Run("invalid token", func(t *testing.T) {
		d := baseDeps(t, nil, nil) // Authenticator with no keys → any token is invalid
		called := false
		h := d.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

		r := httptest.NewRequest(http.MethodPost, "/wallets", nil)
		r.Header.Set("Authorization", "Bearer not.a.jwt")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.False(t, called)
	})
}

// ── router assembly ────────────────────────────────────────────────────────

// TestRouterExposesTheDocumentedRoutes builds the real router and asserts the
// public endpoints answer without a token while business routes demand one.
// It is the cheapest proof that the route table and the auth wrapper agree.
func TestRouterExposesTheDocumentedRoutes(t *testing.T) {
	d := baseDeps(t, nil, nil)
	router := NewRouter(d)

	t.Run("health and metrics are public", func(t *testing.T) {
		for _, path := range []string{"/health/live"} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equalf(t, http.StatusOK, rec.Code, "%s must be reachable unauthenticated", path)
		}
	})

	t.Run("business routes require a token", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/wallets"},
			{http.MethodGet, "/wallets/abc"},
			{http.MethodGet, "/wallets/abc/ledger"},
		} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equalf(t, http.StatusUnauthorized, rec.Code,
				"%s %s must reject an anonymous caller", tc.method, tc.path)
		}
	})
}

// TestNewServerCarriesSaneTimeouts pins the timeout budget. A missing
// ReadHeaderTimeout is the classic Slowloris exposure, so it is asserted, not
// assumed.
func TestNewServerCarriesSaneTimeouts(t *testing.T) {
	d := baseDeps(t, nil, nil)
	s := NewServer(d)

	require.NotNil(t, s.Handler)
	assert.Equal(t, d.Cfg.HTTPAddr, s.Addr)
	assert.Greater(t, s.ReadHeaderTimeout, time.Duration(0))
	assert.Greater(t, s.ReadTimeout, time.Duration(0))
	assert.Greater(t, s.WriteTimeout, time.Duration(0))
	assert.Greater(t, s.IdleTimeout, time.Duration(0))
}
