package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	kithttp "github.com/go-kit/kit/transport/http"

	"github.com/chalkan33/wallet/internal/domain"
	"github.com/chalkan33/wallet/internal/infra/auth"
)

// Context keys populated by middleware and consumed by decoders/endpoints.
type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxIdempotencyKey
	ctxCorrelation
)

func principalFrom(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(ctxPrincipal).(*auth.Principal)
	return p
}

func idempotencyKeyFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxIdempotencyKey).(string)
	return v
}

func correlationFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxCorrelation).(string)
	return v
}

func decodeOpenWallet(_ context.Context, r *http.Request) (interface{}, error) {
	var body struct {
		PlayerID       string    `json:"playerId"`
		InitialBalance moneyWire `json:"initialBalance"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		return nil, err
	}
	if body.PlayerID == "" || body.InitialBalance.Amount == "" || body.InitialBalance.Currency == "" {
		return nil, &validationError{Detail: "playerId and initialBalance{amount,currency} are required"}
	}
	return openWalletRequest{
		PlayerID:      body.PlayerID,
		InitialAmount: body.InitialBalance.Amount,
		Currency:      body.InitialBalance.Currency,
	}, nil
}

func decodeGetWallet(_ context.Context, r *http.Request) (interface{}, error) {
	id := r.PathValue("walletId")
	if id == "" {
		return nil, &validationError{Detail: "walletId is required"}
	}
	return getWalletRequest{WalletID: id}, nil
}

func decodeListLedger(_ context.Context, r *http.Request) (interface{}, error) {
	id := r.PathValue("walletId")
	if id == "" {
		return nil, &validationError{Detail: "walletId is required"}
	}
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	return listLedgerRequest{WalletID: id, Cursor: q.Get("cursor"), Limit: limit}, nil
}

func decodeReconcile(_ context.Context, r *http.Request) (interface{}, error) {
	id := r.PathValue("walletId")
	if id == "" {
		return nil, &validationError{Detail: "walletId is required"}
	}
	return reconcileRequest{WalletID: id}, nil
}

func decodeSubmit(ctx context.Context, r *http.Request) (interface{}, error) {
	var body submitRequest
	if err := decodeJSONBody(r, &body); err != nil {
		return nil, err
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, &validationError{Detail: "Idempotency-Key header is required"}
	}
	// The server never replaces a client key with a computed one.
	_ = ctx
	if body.ProviderID == "" || body.ExternalTransactionID == "" || body.Kind == "" ||
		body.Money.Amount == "" || body.Money.Currency == "" {
		return nil, &validationError{Detail: "providerId, externalTransactionId, kind and money{amount,currency} are required"}
	}
	if body.Kind == string(domain.KindOpening) {
		return nil, &validationError{Detail: "OPENING is internal-only"}
	}
	return body, nil
}

func decodeGetTransaction(_ context.Context, r *http.Request) (interface{}, error) {
	id := r.PathValue("transactionId")
	if id == "" {
		return nil, &validationError{Detail: "transactionId is required"}
	}
	return getTransactionRequest{TransactionID: id}, nil
}

func decodeGetProviderTransaction(_ context.Context, r *http.Request) (interface{}, error) {
	providerID := r.PathValue("providerId")
	externalID := r.PathValue("externalTransactionId")
	if providerID == "" || externalID == "" {
		return nil, &validationError{Detail: "providerId and externalTransactionId are required"}
	}
	return getProviderTransactionRequest{ProviderID: providerID, ExternalTransactionID: externalID}, nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &validationError{Detail: "invalid JSON body: " + err.Error()}
	}
	return nil
}

type validationError struct{ Detail string }

func (e *validationError) Error() string { return "VALIDATION_FAILED: " + e.Detail }

// kithttp options shared by every route.
func serverOptions(errEnc kithttp.ErrorEncoder) []kithttp.ServerOption {
	return []kithttp.ServerOption{
		kithttp.ServerErrorEncoder(errEnc),
	}
}
