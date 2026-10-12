// Package httpapi is the go-kit HTTP transport: decoders bind and validate
// wire input, endpoints call the application use cases, encoders write
// responses, and the error encoder maps classified errors to stable HTTP
// codes and bodies.
package httpapi

import (
	"context"
	"time"

	"github.com/go-kit/kit/endpoint"

	"github.com/chalkan3/backend-challenge-go/internal/app"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
)

// Endpoints bundles every go-kit endpoint.
type Endpoints struct {
	OpenWallet             endpoint.Endpoint
	GetWallet              endpoint.Endpoint
	ListLedger             endpoint.Endpoint
	Reconcile              endpoint.Endpoint
	SubmitTransaction      endpoint.Endpoint
	GetTransaction         endpoint.Endpoint
	GetProviderTransaction endpoint.Endpoint
}

// NewEndpoints builds the endpoints with the go-kit endpoint middleware
// chain: authorization first, then instrumentation.
func NewEndpoints(ws *app.WalletService, wg *app.WagerService, m *metrics.Metrics) Endpoints {
	return Endpoints{
		OpenWallet:             instrument("open_wallet", m)(authorize(openWalletEndpoint(ws), true, false)),
		GetWallet:              instrument("get_wallet", m)(authorize(getWalletEndpoint(ws), true, false)),
		ListLedger:             instrument("list_ledger", m)(authorize(listLedgerEndpoint(ws), true, false)),
		Reconcile:              instrument("reconcile", m)(authorize(reconcileEndpoint(ws), true, false)),
		SubmitTransaction:      instrument("submit_transaction", m)(authorize(submitEndpoint(wg), false, true)),
		GetTransaction:         instrument("get_transaction", m)(authorize(getTransactionEndpoint(wg), false, false)),
		GetProviderTransaction: instrument("get_provider_transaction", m)(authorize(getProviderTransactionEndpoint(wg), false, false)),
	}
}

// instrument is the go-kit endpoint middleware for latency and outcomes.
func instrument(name string, m *metrics.Metrics) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, request interface{}) (response interface{}, err error) {
			begin := time.Now()
			defer func() {
				m.RequestLatency.With("endpoint", name).Observe(time.Since(begin).Seconds())
				m.RequestCount.With("endpoint", name, "success", boolStr(err == nil), "error", errorCode(err)).Add(1)
			}()
			return next(ctx, request)
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if he, ok := isHTTPError(err); ok {
		return he.Code
	}
	switch {
	case app.IsValidation(err):
		return app.CodeValidationFailed
	case app.IsNotFound(err):
		return app.NotFoundCode(err)
	case app.IsConflict(err):
		return app.ConflictCode(err)
	case app.IsUnavailable(err):
		return app.CodeTransient
	default:
		return "INTERNAL_ERROR"
	}
}

// authorize enforces the authorization model at the endpoint layer:
// internal=true restricts the endpoint to the internal service identity;
// providerScoped endpoints check the providerId against the authenticated
// identity inside the endpoint body.
func authorize(next endpoint.Endpoint, internalOnly, providerScoped bool) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		p := principalFrom(ctx)
		if p == nil {
			return nil, errUnauthorized("missing authenticated identity")
		}
		if internalOnly && !p.Internal {
			return nil, errForbidden("wallet operations are restricted to the internal service")
		}
		if providerScoped && !p.Internal && p.ProviderID == "" {
			return nil, errForbidden("provider identity required")
		}
		return next(ctx, request)
	}
}
