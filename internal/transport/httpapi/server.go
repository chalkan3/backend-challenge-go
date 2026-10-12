package httpapi

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

	"github.com/chalkan33/wallet/internal/infra/auth"
	"github.com/chalkan33/wallet/internal/infra/sqs"
	"github.com/chalkan33/wallet/internal/platform/config"
)

// Deps carries everything the HTTP layer needs.
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

// NewRouter mounts every route. Business routes require a valid bearer
// token (OAuth2/OIDC); only health checks and /metrics are public.
func NewRouter(d Deps) http.Handler {
	opts := serverOptions(makeErrorEncoder())
	kit := func(e endpoint.Endpoint, dec kithttp.DecodeRequestFunc, enc kithttp.EncodeResponseFunc) http.Handler {
		return kithttp.NewServer(e, dec, enc, opts...)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /wallets", d.auth(kit(d.Ep.OpenWallet, decodeOpenWallet, encodeJSONWithStatus(http.StatusCreated))))
	mux.Handle("GET /wallets/{walletId}", d.auth(kit(d.Ep.GetWallet, decodeGetWallet, encodeJSONWithStatus(http.StatusOK))))
	mux.Handle("GET /wallets/{walletId}/ledger", d.auth(kit(d.Ep.ListLedger, decodeListLedger, encodeJSONWithStatus(http.StatusOK))))
	mux.Handle("POST /wallets/{walletId}/reconciliation", d.auth(kit(d.Ep.Reconcile, decodeReconcile, encodeJSONWithStatus(http.StatusOK))))
	mux.Handle("POST /wagering/transactions", d.auth(kit(d.Ep.SubmitTransaction, decodeSubmit, encodeSubmit)))
	mux.Handle("GET /wagering/transactions/{transactionId}", d.auth(kit(d.Ep.GetTransaction, decodeGetTransaction, encodeJSONWithStatus(http.StatusOK))))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		d.auth(kit(d.Ep.GetProviderTransaction, decodeGetProviderTransaction, encodeJSONWithStatus(http.StatusOK))))

	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	mux.HandleFunc("GET /health/ready", d.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(d.Reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown route")
	})
	return d.logRequests(mux)
}

// auth validates the bearer token and injects the Principal plus transport
// context (idempotency key, correlation id).
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

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// ready implements GET /health/ready: PostgreSQL and SQS reachability.
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
