// Package http is the go-kit HTTP transport of the wallet service: request
// decoders bind and validate wire input, endpoints call the application use
// cases through go-kit endpoint middleware (authorization → instrumentation,
// both Decorators), response encoders write JSON, and the error encoder maps
// classified errors to stable HTTP codes and bodies.
package http

import (
	"context"
	"time"

	"github.com/go-kit/kit/endpoint"

	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Endpoints bundles every go-kit endpoint exposed over HTTP.
type Endpoints struct {
	OpenWallet             endpoint.Endpoint
	GetWallet              endpoint.Endpoint
	ListLedger             endpoint.Endpoint
	Reconcile              endpoint.Endpoint
	SubmitTransaction      endpoint.Endpoint
	GetTransaction         endpoint.Endpoint
	GetProviderTransaction endpoint.Endpoint
}

// NewEndpoints builds each endpoint behind the same middleware chain
// (Decorator pattern): authorization runs first, instrumentation wraps the
// outcome of everything inside it — including authorization failures.
func NewEndpoints(ws *service.WalletService, wg *service.WagerService, m *metrics.Metrics) Endpoints {
	return Endpoints{
		OpenWallet:             instrument("open_wallet", m)(authorize(m, openWalletEndpoint(ws), true, false)),
		GetWallet:              instrument("get_wallet", m)(authorize(m, getWalletEndpoint(ws), true, false)),
		ListLedger:             instrument("list_ledger", m)(authorize(m, listLedgerEndpoint(ws), true, false)),
		Reconcile:              instrument("reconcile", m)(authorize(m, reconcileEndpoint(ws), true, false)),
		SubmitTransaction:      instrument("submit_transaction", m)(authorize(m, submitEndpoint(wg), false, true)),
		GetTransaction:         instrument("get_transaction", m)(authorize(m, getTransactionEndpoint(wg), false, false)),
		GetProviderTransaction: instrument("get_provider_transaction", m)(authorize(m, getProviderTransactionEndpoint(wg), false, false)),
	}
}

// instrument is the go-kit endpoint middleware for latency and outcomes: it
// observes the endpoint latency histogram and increments the request counter
// with the stable error code (empty on success).
func instrument(name string, m *metrics.Metrics) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (resp interface{}, err error) {
			defer func(begin time.Time) {
				m.RequestLatency.With("endpoint", name).Observe(time.Since(begin).Seconds())
				m.RequestCount.With("endpoint", name, "success", boolStr(err == nil), "error", errorCode(err)).Add(1)
			}(time.Now())
			return next(ctx, request)
		}
	}
}

// authorize is the go-kit endpoint middleware enforcing the two access rules
// of the API surface:
//   - internal=true restricts the endpoint to the internal service identity;
//   - providerScoped endpoints require a provider identity (the providerId
//     match itself is checked inside the endpoint body).
//
// Every rejection is counted on wallet_auth_failures_total with a stable
// reason before the error is surfaced to the error encoder.
func authorize(m *metrics.Metrics, next endpoint.Endpoint, internalOnly, providerScoped bool) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		p := principalFrom(ctx)
		switch {
		case p == nil:
			m.AuthFailures.With("reason", "missing_identity").Add(1)
			return nil, errUnauthorized("missing authenticated identity")
		case internalOnly && !p.Internal:
			m.AuthFailures.With("reason", "wallet_requires_internal_identity").Add(1)
			return nil, errForbidden("wallet operations are restricted to the internal service")
		case providerScoped && !p.Internal && p.ProviderID == "":
			m.AuthFailures.With("reason", "provider_identity_required").Add(1)
			return nil, errForbidden("provider identity required")
		}
		return next(ctx, request)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// errorCode reduces any endpoint failure to a stable, bounded label value
// for the requests_total series.
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if he, ok := isHTTPError(err); ok {
		return he.Code
	}
	switch {
	case isValidationErr(err), service.IsValidation(err):
		return service.CodeValidationFailed
	case service.IsNotFound(err):
		return service.NotFoundCode(err)
	case service.IsConflict(err):
		return service.ConflictCode(err)
	case service.IsUnavailable(err):
		return service.CodeTransient
	default:
		return "INTERNAL_ERROR"
	}
}
