// Package http is the go-kit HTTP transport of the wallet service.
package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/go-kit/kit/endpoint"
	kithttp "github.com/go-kit/kit/transport/http"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// Deps carries everything the HTTP transport needs. It is assembled once by
// the composition root (bootstrap) and consumed read-only.
type Deps struct {
	Cfg  *config.Config
	Log  *slog.Logger
	Auth *auth.Authenticator
	Ep   Endpoints
	Pool *pgxpool.Pool
	SQS  *awssqs.Client
	URLs sqs.QueueURLs
	Reg  *prometheus.Registry
}

// NewServer builds the *http.Server (lifecycle is managed by Fx).
func NewServer(d Deps) *http.Server {
	return &http.Server{
		Addr:              d.Cfg.HTTPAddr,
		Handler:           NewRouter(d),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// route is one entry of the declarative routing table.
type route struct {
	method  string // Go 1.22+ method-pattern ("POST /wallets")
	handler http.Handler
}

// NewRouter mounts every route. Business routes require a valid bearer
// token (OAuth2/OIDC); only health checks and /metrics are public.
// Everything is wrapped by the access-log Decorator.
func NewRouter(d Deps) http.Handler {
	kit := func(e endpoint.Endpoint, dec kithttp.DecodeRequestFunc, enc kithttp.EncodeResponseFunc) http.Handler {
		return kithttp.NewServer(e, dec, enc, serverOptions(makeErrorEncoder())...)
	}

	business := []route{
		{method: "POST /wallets",
			handler: d.auth(kit(d.Ep.OpenWallet, decodeOpenWallet, encodeJSONWithStatus(http.StatusCreated)))},
		{method: "GET /wallets/{walletId}",
			handler: d.auth(kit(d.Ep.GetWallet, decodeGetWallet, encodeJSONWithStatus(http.StatusOK)))},
		{method: "GET /wallets/{walletId}/ledger",
			handler: d.auth(kit(d.Ep.ListLedger, decodeListLedger, encodeJSONWithStatus(http.StatusOK)))},
		{method: "POST /wallets/{walletId}/reconciliation",
			handler: d.auth(kit(d.Ep.Reconcile, decodeReconcile, encodeJSONWithStatus(http.StatusOK)))},
		{method: "POST /wagering/transactions",
			handler: d.auth(kit(d.Ep.SubmitTransaction, decodeSubmit, encodeSubmit))},
		{method: "GET /wagering/transactions/{transactionId}",
			handler: d.auth(kit(d.Ep.GetTransaction, decodeGetTransaction, encodeJSONWithStatus(http.StatusOK)))},
		{method: "GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
			handler: d.auth(kit(d.Ep.GetProviderTransaction, decodeGetProviderTransaction, encodeJSONWithStatus(http.StatusOK)))},
	}

	public := []route{
		{method: "GET /health/live", handler: http.HandlerFunc(handleLive)},
		{method: "GET /health/ready", handler: http.HandlerFunc(d.ready)},
		{method: "GET /metrics", handler: promhttp.HandlerFor(d.Reg, promhttp.HandlerOpts{})},
	}

	mux := http.NewServeMux()
	for _, r := range append(business, public...) {
		mux.Handle(r.method, r.handler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown route")
	})
	return d.logRequests(mux)
}

// handleLive is the liveness probe: the process is up; no dependency check.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"status":"live"}`))
}

// auth validates the bearer token and injects the Principal plus transport
// context (idempotency key, correlation id). It is the HTTP-level half of
// authentication; coarse-grained authorization happens as an endpoint
// middleware (see authorize).
func (d Deps) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		p, err := d.Auth.Validate(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxPrincipal, &p)
		ctx = context.WithValue(ctx, ctxIdempotencyKey, r.Header.Get("Idempotency-Key"))
		correlation := r.Header.Get("X-Correlation-Id")
		if correlation == "" {
			correlation = uuid.NewString()
		}
		w.Header().Set("X-Correlation-Id", correlation)
		ctx = context.WithValue(ctx, ctxCorrelation, correlation)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// logRequests emits one JSON access log line per request.
func (d Deps) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/health/live" { // keep logs lean
			return
		}
		d.Log.Info("http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("correlationId", sw.Header().Get("X-Correlation-Id")))
	})
}

// statusWriter captures the response status for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// bearerToken extracts the raw token from an Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// ready implements GET /health/ready: PostgreSQL and SQS reachability.
// Any unreachable dependency yields 503 so the orchestrator withholds
// traffic from this instance.
func (d Deps) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	ready := true
	if err := d.Pool.Ping(ctx); err != nil {
		ready = false
		d.Log.Warn("readiness: postgres unreachable", slog.String("error", err.Error()))
	}
	if _, err := d.SQS.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(d.Cfg.SQSRequestQueue)}); err != nil {
		ready = false
		d.Log.Warn("readiness: sqs unreachable", slog.String("error", err.Error()))
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":"` + state + `"}`))
}
