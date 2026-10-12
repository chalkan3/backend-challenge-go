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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
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
	OTel *otel.Provider
	Met  *metrics.Metrics
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
	// Routes are mounted regardless of mode. In workers mode the process
	// still serves /health/* and /metrics (probes and scraping must not
	// depend on the role), but the business routes stay unregistered so a
	// misrouted request gets an honest 404 instead of being handled by a
	// process that was never meant to take traffic.
	routes := public
	if d.Cfg.Mode.ServesBusinessRoutes() {
		routes = append(business, public...)
	}
	for _, r := range routes {
		mux.Handle(r.method, r.handler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown route")
	})
	// Middleware order (outermost first):
	//   tracing → access log → routes
	// Tracing is outermost so the span covers the whole request — including
	// the access-log line, which then carries the trace_id — and so that
	// /health/live, though unlogged, is still traced for uptime visibility.
	// otelhttp also extracts the incoming W3C traceparent, which is what
	// stitches a provider's upstream trace onto ours.
	return d.tracing(d.logRequests(mux))
}

// tracing wraps the router with the OpenTelemetry HTTP handler. The provider
// is never nil for production wiring, but Instrument never assumes it: a
// disabled provider installs a no-op tracer, so the same call site serves
// every deployment.
func (d Deps) tracing(next http.Handler) http.Handler {
	h := otelhttp.NewHandler(next, "http.server",
		otelhttp.WithTracerProvider(d.OTel.TracerProvider()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			// Method+route-pattern would be ideal, but the pattern is only
			// known after ServeMux matches; the concrete path is bounded
			// enough (IDs appear only in the leaf segment).
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/metrics" // scraping would drown the traces
		}),
	)
	return h
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
//
// Rejections are counted here, not only in authorize: a missing or invalid
// token never reaches the endpoint middleware, so counting solely there made
// wallet_auth_failures_total blind to the most frequent failure of all.
func (d Deps) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			d.Met.AuthFailures.With("reason", "missing_bearer_token").Add(1)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		p, err := d.Auth.Validate(raw)
		if err != nil {
			d.Met.AuthFailures.With("reason", "invalid_bearer_token").Add(1)
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

// logRequests emits one JSON access log line per request. When tracing is
// active the line carries trace_id/span_id, which is the join key between a
// log line and its trace (and, via the Collector, between Loki/Tempo).
func (d Deps) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/health/live" { // keep logs lean
			return
		}
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("correlationId", sw.Header().Get("X-Correlation-Id")),
		}
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			attrs = append(attrs,
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()))
		}
		d.Log.Info("http request", attrs...)
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
