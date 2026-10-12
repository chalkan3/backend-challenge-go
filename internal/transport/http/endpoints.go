package http

import (
	"context"
	"errors"
	"time"

	"github.com/go-kit/kit/endpoint"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Requests/responses and endpoint bodies for the go-kit transport.
// Decoders (codecs.go) build the request structs; endpoints call the use
// cases; encoders (errors.go) write the JSON.

// --- requests --------------------------------------------------------------

type openWalletRequest struct {
	PlayerID      string     `json:"playerId"`
	InitialAmount string     `json:"initialAmount"`
	InitialObject *moneyWire `json:"initialBalance,omitempty"`
	Currency      string
	CorrelationID string
}

type moneyWire struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type getWalletRequest struct{ WalletID string }

type listLedgerRequest struct {
	WalletID string
	Cursor   string
	Limit    int
}

type reconcileRequest struct{ WalletID string }

type submitRequest struct {
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	PlayerID                       string    `json:"playerId"`
	WalletID                       string    `json:"walletId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	Kind                           string    `json:"kind"`
	Money                          moneyWire `json:"money"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId,omitempty"`
}

type getTransactionRequest struct{ TransactionID string }

type getProviderTransactionRequest struct {
	ProviderID            string
	ExternalTransactionID string
}

// --- responses -------------------------------------------------------------

type walletResponse struct {
	ID        string    `json:"id"`
	PlayerID  string    `json:"playerId"`
	Balance   moneyWire `json:"balance"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type submitResponse struct {
	TransactionID    string     `json:"transactionId"`
	Status           string     `json:"status"`
	Balance          moneyWire  `json:"balance"`
	IdempotentReplay bool       `json:"idempotentReplay"`
	FailureCode      string     `json:"failureCode,omitempty"`
	FailureMessage   string     `json:"failureMessage,omitempty"`
	NextRetryAt      *time.Time `json:"nextRetryAt,omitempty"`
}

type transactionResponse struct {
	TransactionID                  string     `json:"transactionId"`
	Kind                           string     `json:"kind"`
	Origin                         string     `json:"origin"`
	ProviderID                     string     `json:"providerId,omitempty"`
	ExternalTransactionID          string     `json:"externalTransactionId,omitempty"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId,omitempty"`
	GameID                         string     `json:"gameId,omitempty"`
	Amount                         moneyWire  `json:"amount"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
	Status                         string     `json:"status"`
	FailureCode                    string     `json:"failureCode,omitempty"`
	FailureMessage                 string     `json:"failureMessage,omitempty"`
	ResultDirection                string     `json:"resultDirection,omitempty"`
	ResultAmount                   *moneyWire `json:"resultAmount,omitempty"`
	BalanceBefore                  *moneyWire `json:"balanceBefore,omitempty"`
	BalanceAfter                   *moneyWire `json:"balanceAfter,omitempty"`
	WalletVersionAfter             *int64     `json:"walletVersionAfter,omitempty"`
	Channel                        string     `json:"channel"`
	CreatedAt                      time.Time  `json:"createdAt"`
	UpdatedAt                      time.Time  `json:"updatedAt"`
}

type ledgerResponse struct {
	Items      []ledgerItemResponse `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

type ledgerItemResponse struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	Direction     string    `json:"direction"`
	Amount        moneyWire `json:"amount"`
	BalanceBefore moneyWire `json:"balanceBefore"`
	BalanceAfter  moneyWire `json:"balanceAfter"`
	CreatedAt     time.Time `json:"createdAt"`
}

type reconciliationResponse struct {
	WalletID          string    `json:"walletId"`
	StoredBalance     moneyWire `json:"storedBalance"`
	CalculatedBalance moneyWire `json:"calculatedBalance"`
	Difference        moneyWire `json:"difference"`
	Consistent        bool      `json:"consistent"`
	CheckedEntries    int64     `json:"checkedEntries"`
}

// --- endpoint bodies -------------------------------------------------------

func openWalletEndpoint(ws *service.WalletService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(openWalletRequest)
		in := service.OpenWalletInput{
			PlayerID:      req.PlayerID,
			InitialAmount: req.InitialAmount,
			Currency:      req.Currency,
			CorrelationID: req.CorrelationID,
		}
		view, err := ws.Open(ctx, in)
		if err != nil {
			return nil, err
		}
		return walletResponse{
			ID: view.ID.String(), PlayerID: view.PlayerID.String(),
			Balance: moneyPayload(view.Balance), Version: view.Version,
			CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
		}, nil
	}
}

func getWalletEndpoint(ws *service.WalletService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(getWalletRequest)
		view, err := ws.Get(ctx, req.WalletID)
		if err != nil {
			return nil, err
		}
		return walletResponse{
			ID: view.ID.String(), PlayerID: view.PlayerID.String(),
			Balance: moneyPayload(view.Balance), Version: view.Version,
			CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
		}, nil
	}
}

func listLedgerEndpoint(ws *service.WalletService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(listLedgerRequest)
		items, next, err := ws.ListLedger(ctx, req.WalletID, req.Cursor, req.Limit)
		if err != nil {
			return nil, err
		}
		out := ledgerResponse{Items: make([]ledgerItemResponse, 0, len(items)), NextCursor: next}
		for _, it := range items {
			out.Items = append(out.Items, ledgerItemResponse{
				ID: it.ID, TransactionID: it.TransactionID,
				Direction:     string(it.Direction),
				Amount:        moneyPayload(it.Amount),
				BalanceBefore: moneyPayload(it.BalanceBefore),
				BalanceAfter:  moneyPayload(it.BalanceAfter),
				CreatedAt:     it.CreatedAt,
			})
		}
		return out, nil
	}
}

func reconcileEndpoint(ws *service.WalletService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(reconcileRequest)
		rep, err := ws.Reconcile(ctx, req.WalletID)
		if err != nil {
			return nil, err
		}
		return reconciliationResponse{
			WalletID:          rep.WalletID.String(),
			StoredBalance:     moneyPayload(rep.StoredBalance),
			CalculatedBalance: moneyPayload(rep.CalculatedBalance),
			Difference:        moneyPayload(rep.Difference),
			Consistent:        rep.Consistent,
			CheckedEntries:    rep.CheckedEntries,
		}, nil
	}
}

func submitEndpoint(wg *service.WagerService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(submitRequest)
		p := principalFrom(ctx)
		// Provider isolation: a provider identity may only submit for itself.
		if p != nil && !p.Internal && p.ProviderID != req.ProviderID {
			return nil, errForbidden("token is not authorized for provider " + req.ProviderID)
		}
		in := service.ExternalInput{
			ProviderID:                     req.ProviderID,
			ExternalTransactionID:          req.ExternalTransactionID,
			IdempotencyKey:                 idempotencyKeyFrom(ctx),
			PlayerID:                       req.PlayerID,
			WalletID:                       req.WalletID,
			RoundID:                        req.RoundID,
			GameID:                         req.GameID,
			Kind:                           req.Kind,
			Amount:                         req.Money.Amount,
			Currency:                       req.Money.Currency,
			ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
			ReceivedVia:                    domain.ViaHTTP,
			CorrelationID:                  correlationFrom(ctx),
		}
		res, err := wg.Submit(ctx, in)
		if err != nil {
			return nil, err
		}
		return buildSubmitResponse(res), nil
	}
}

func buildSubmitResponse(res service.WagerResult) submitResponse {
	t := res.Transaction
	out := submitResponse{
		TransactionID:    t.ID().String(),
		Status:           string(t.Status()),
		Balance:          moneyPayload(res.WalletBalance),
		IdempotentReplay: res.IdempotentReplay,
	}
	out.FailureCode = t.FailureCode()
	out.FailureMessage = t.FailureMessage()
	out.NextRetryAt = t.NextRetryAt()
	return out
}

func getTransactionEndpoint(wg *service.WagerService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(getTransactionRequest)
		t, err := wg.GetByID(ctx, req.TransactionID)
		if err != nil {
			return nil, err
		}
		// Providers see only their own transactions, including replays.
		if p := principalFrom(ctx); p != nil && !p.Internal {
			if t.ProviderID() == "" || t.ProviderID() != p.ProviderID {
				return nil, errForbidden("transaction belongs to another provider")
			}
		}
		return transactionPayload(t), nil
	}
}

func getProviderTransactionEndpoint(wg *service.WagerService) endpoint.Endpoint {
	return func(ctx context.Context, request interface{}) (interface{}, error) {
		req := request.(getProviderTransactionRequest)
		if p := principalFrom(ctx); p != nil && !p.Internal && p.ProviderID != req.ProviderID {
			return nil, errForbidden("token is not authorized for provider " + req.ProviderID)
		}
		t, err := wg.GetByProviderExternal(ctx, req.ProviderID, req.ExternalTransactionID)
		if err != nil {
			return nil, err
		}
		return transactionPayload(t), nil
	}
}

func transactionPayload(t *domain.WagerTransaction) transactionResponse {
	out := transactionResponse{
		TransactionID: t.ID().String(),
		Kind:          string(t.Kind()),
		Origin:        string(t.Origin()),
		PlayerID:      t.PlayerID().String(),
		WalletID:      t.WalletID().String(),
		Amount:        moneyPayload(t.Amount()),
		Status:        string(t.Status()),
		Channel:       string(t.ReceivedVia()),
		CreatedAt:     t.CreatedAt(),
		UpdatedAt:     t.UpdatedAt(),
	}
	if t.ProviderID() != "" {
		out.ProviderID = t.ProviderID()
		out.ExternalTransactionID = t.ExternalTxID()
	}
	if t.RoundID() != "" {
		out.RoundID = t.RoundID()
		out.GameID = t.GameID()
	}
	if t.ReferenceExternalTxID() != "" {
		out.ReferenceExternalTransactionID = t.ReferenceExternalTxID()
	}
	out.FailureCode = t.FailureCode()
	out.FailureMessage = t.FailureMessage()
	if t.ResultDirection() != domain.DirectionNone {
		out.ResultDirection = string(t.ResultDirection())
		ra := moneyPayload(t.ResultAmount())
		out.ResultAmount = &ra
	}
	if t.BalanceBefore() != nil {
		bb := moneyPayload(*t.BalanceBefore())
		out.BalanceBefore = &bb
	}
	if t.BalanceAfter() != nil {
		ba := moneyPayload(*t.BalanceAfter())
		out.BalanceAfter = &ba
	}
	out.WalletVersionAfter = t.WalletVersionAfter()
	return out
}

func moneyPayload(m domain.Money) moneyWire {
	return moneyWire{Amount: m.String(), Currency: m.Currency()}
}

// --- transport-level errors ------------------------------------------------

type httpError struct {
	Status int
	Code   string
	Detail string
}

func (e *httpError) Error() string { return e.Code + ": " + e.Detail }

func errUnauthorized(d string) error { return &httpError{401, service.CodeUnauthorized, d} }
func errForbidden(d string) error    { return &httpError{403, service.CodeForbidden, d} }

func isHTTPError(err error) (*httpError, bool) {
	var he *httpError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}
