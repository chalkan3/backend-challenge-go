// These white-box tests exercise the HTTP adapter's pure parts: every request
// decoder, the error encoder's status mapping, the response encoders and the
// payload builders. The go-kit wiring these feed into is covered end-to-end by
// the integration suite; here the individual contracts are pinned without a
// server, so a decoder that stops rejecting malformed input fails fast.
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// --- context helpers --------------------------------------------------------

func TestContextAccessors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assert.Nil(t, principalFrom(ctx))
	assert.Empty(t, idempotencyKeyFrom(ctx))
	assert.Empty(t, correlationFrom(ctx))

	ctx = context.WithValue(ctx, ctxPrincipal, &auth.Principal{ClientID: "c"})
	ctx = context.WithValue(ctx, ctxIdempotencyKey, "idem")
	ctx = context.WithValue(ctx, ctxCorrelation, "corr")
	require.NotNil(t, principalFrom(ctx))
	assert.Equal(t, "c", principalFrom(ctx).ClientID)
	assert.Equal(t, "idem", idempotencyKeyFrom(ctx))
	assert.Equal(t, "corr", correlationFrom(ctx))
}

// --- decoders ---------------------------------------------------------------

func newReq(method, target, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestDecodeOpenWallet(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), ctxIdempotencyKey, "k")

	t.Run("valid", func(t *testing.T) {
		r := newReq("POST", "/wallets", `{"playerId":"p","initialBalance":{"amount":"10.00","currency":"BRL"}}`, nil)
		got, err := decodeOpenWallet(ctx, r)
		require.NoError(t, err)
		req := got.(openWalletRequest)
		assert.Equal(t, "p", req.PlayerID)
		assert.Equal(t, "10.00", req.InitialAmount)
		assert.Equal(t, "BRL", req.Currency)
		assert.Equal(t, "k", req.IdempotencyKey)
	})

	t.Run("missing fields", func(t *testing.T) {
		r := newReq("POST", "/wallets", `{"playerId":"p"}`, nil)
		_, err := decodeOpenWallet(ctx, r)
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})

	t.Run("malformed json", func(t *testing.T) {
		r := newReq("POST", "/wallets", `{`, nil)
		_, err := decodeOpenWallet(ctx, r)
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})
}

func TestDecodePathIDDecoders(t *testing.T) {
	t.Parallel()

	t.Run("get wallet", func(t *testing.T) {
		r := newReq("GET", "/wallets/abc", "", nil)
		r.SetPathValue("walletId", "abc")
		got, err := decodeGetWallet(context.Background(), r)
		require.NoError(t, err)
		assert.Equal(t, getWalletRequest{WalletID: "abc"}, got)

		r2 := newReq("GET", "/wallets/", "", nil)
		_, err = decodeGetWallet(context.Background(), r2)
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})

	t.Run("reconcile", func(t *testing.T) {
		r := newReq("GET", "/wallets/abc/reconciliation", "", nil)
		r.SetPathValue("walletId", "abc")
		got, err := decodeReconcile(context.Background(), r)
		require.NoError(t, err)
		assert.Equal(t, reconcileRequest{WalletID: "abc"}, got)

		r2 := newReq("GET", "/", "", nil)
		_, err = decodeReconcile(context.Background(), r2)
		require.Error(t, err)
	})
}

func TestDecodeListLedger(t *testing.T) {
	t.Parallel()

	t.Run("defaults limit", func(t *testing.T) {
		r := newReq("GET", "/wallets/w1/ledger", "", nil)
		r.SetPathValue("walletId", "w1")
		got, err := decodeListLedger(context.Background(), r)
		require.NoError(t, err)
		assert.Equal(t, 50, got.(listLedgerRequest).Limit)
	})

	t.Run("honours limit and cursor", func(t *testing.T) {
		r := newReq("GET", "/wallets/w1/ledger?limit=7&cursor=cur", "", nil)
		r.SetPathValue("walletId", "w1")
		got, err := decodeListLedger(context.Background(), r)
		require.NoError(t, err)
		req := got.(listLedgerRequest)
		assert.Equal(t, 7, req.Limit)
		assert.Equal(t, "cur", req.Cursor)
	})

	t.Run("non-numeric limit falls back", func(t *testing.T) {
		r := newReq("GET", "/wallets/w1/ledger?limit=abc", "", nil)
		r.SetPathValue("walletId", "w1")
		got, err := decodeListLedger(context.Background(), r)
		require.NoError(t, err)
		assert.Equal(t, 50, got.(listLedgerRequest).Limit)
	})

	t.Run("non-positive limit falls back", func(t *testing.T) {
		r := newReq("GET", "/wallets/w1/ledger?limit=-3", "", nil)
		r.SetPathValue("walletId", "w1")
		got, err := decodeListLedger(context.Background(), r)
		require.NoError(t, err)
		assert.Equal(t, 50, got.(listLedgerRequest).Limit)
	})

	t.Run("missing wallet id", func(t *testing.T) {
		r := newReq("GET", "/ledger", "", nil)
		_, err := decodeListLedger(context.Background(), r)
		require.Error(t, err)
	})
}

func TestDecodeGetTransaction(t *testing.T) {
	t.Parallel()
	r := newReq("GET", "/transactions/t1", "", nil)
	r.SetPathValue("transactionId", "t1")
	got, err := decodeGetTransaction(context.Background(), r)
	require.NoError(t, err)
	assert.Equal(t, getTransactionRequest{TransactionID: "t1"}, got)

	r2 := newReq("GET", "/transactions", "", nil)
	_, err = decodeGetTransaction(context.Background(), r2)
	require.Error(t, err)
}

func TestDecodeGetProviderTransaction(t *testing.T) {
	t.Parallel()
	r := newReq("GET", "/providers/provider-a/transactions/ext-1", "", nil)
	r.SetPathValue("providerId", "provider-a")
	r.SetPathValue("externalTransactionId", "ext-1")
	got, err := decodeGetProviderTransaction(context.Background(), r)
	require.NoError(t, err)
	req := got.(getProviderTransactionRequest)
	assert.Equal(t, "provider-a", req.ProviderID)
	assert.Equal(t, "ext-1", req.ExternalTransactionID)

	r2 := newReq("GET", "/providers//transactions/", "", nil)
	_, err = decodeGetProviderTransaction(context.Background(), r2)
	require.Error(t, err)
}

func TestDecodeSubmitRejectsOpeningKind(t *testing.T) {
	t.Parallel()
	body := `{"providerId":"provider-a","externalTransactionId":"e","kind":"OPENING",` +
		`"money":{"amount":"1.00","currency":"BRL"}}`
	r := newReq("POST", "/wagering/transactions", body, map[string]string{"Idempotency-Key": "k"})
	_, err := decodeSubmit(context.Background(), r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPENING is internal-only")
}

func TestDecodeJSONBodyRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var dst struct {
		A string `json:"a"`
	}
	r := newReq("POST", "/", `{"a":"x","b":"y"}`, nil)
	err := decodeJSONBody(r, &dst)
	require.Error(t, err)
	assert.True(t, isValidationErr(err))
	assert.Contains(t, err.Error(), "invalid JSON body")
}

// --- error encoder ----------------------------------------------------------

func TestErrorEncoderStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"http error", errForbidden("nope"), http.StatusForbidden, service.CodeForbidden},
		{"validation", &validationError{Detail: "bad"}, http.StatusBadRequest, service.CodeValidationFailed},
		{"service validation", &service.ValidationError{Detail: "x"}, http.StatusBadRequest, service.CodeValidationFailed},
		{"not found", &service.NotFoundError{Code: service.CodeWalletNotFound, Detail: "w"}, http.StatusNotFound, service.CodeWalletNotFound},
		{"conflict", &service.ConflictError{Code: service.CodeIdemKeyConflict, Detail: "c"}, http.StatusConflict, service.CodeIdemKeyConflict},
		{"unavailable", &service.UnavailableError{Cause: assert.AnError}, http.StatusServiceUnavailable, service.CodeTransient},
		{"unknown", assert.AnError, http.StatusInternalServerError, "INTERNAL_ERROR"},
	}

	enc := makeErrorEncoder()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			enc(context.Background(), tc.err, rec)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.wantCode, body.Error.Code)
		})
	}
}

func TestHTTPErrorHelpers(t *testing.T) {
	t.Parallel()
	ue := errUnauthorized("missing")
	he, ok := isHTTPError(ue)
	require.True(t, ok)
	assert.Equal(t, http.StatusUnauthorized, he.Status)
	assert.Equal(t, service.CodeUnauthorized, he.Code)
	assert.Equal(t, "UNAUTHORIZED: missing", ue.Error())

	_, ok = isHTTPError(assert.AnError)
	assert.False(t, ok)
}

// --- response encoders ------------------------------------------------------

func TestEncodeJSONWithStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	enc := encodeJSONWithStatus(http.StatusCreated)
	require.NoError(t, enc(context.Background(), rec, map[string]string{"id": "1"}))
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"1"`)
}

func TestEncodeSubmitStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status     string
		wantStatus int
	}{
		{"PROCESSED", http.StatusOK},
		{"PENDING_REFERENCE", http.StatusAccepted},
		{"PENDING", http.StatusAccepted},
		{"REJECTED", http.StatusUnprocessableEntity},
		{"FAILED", http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			require.NoError(t, encodeSubmit(context.Background(), rec,
				submitResponse{TransactionID: "t", Status: tc.status}))
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

// --- payload builders -------------------------------------------------------

func TestMoneyPayload(t *testing.T) {
	t.Parallel()
	m, err := domain.NewMoney(1050, "BRL")
	require.NoError(t, err)
	got := moneyPayload(m)
	assert.Equal(t, "10.50", got.Amount)
	assert.Equal(t, "BRL", got.Currency)
}

func TestTransactionPayloadOmitsEmptyOptionals(t *testing.T) {
	t.Parallel()
	tx := rehydrate(t, domain.RehydrateParams{
		Kind:            domain.KindBet,
		Origin:          domain.OriginExternal,
		PlayerID:        uuid.New(),
		WalletID:        uuid.New(),
		Currency:        "BRL",
		AmountMinor:     1000,
		Status:          domain.StatusPending,
		ReceivedVia:     domain.ViaHTTP,
		ResultDirection: domain.DirectionNone,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	})

	out := transactionPayload(tx)
	assert.Equal(t, string(domain.KindBet), out.Kind)
	assert.Empty(t, out.ProviderID, "no provider → omitted")
	assert.Empty(t, out.RoundID, "no round → omitted")
	assert.Empty(t, out.ReferenceExternalTransactionID)
	assert.Nil(t, out.ResultAmount, "no movement → no result amount")
	assert.Nil(t, out.BalanceBefore)
	assert.Nil(t, out.BalanceAfter)
}

func TestTransactionPayloadIncludesPopulatedOptionals(t *testing.T) {
	t.Parallel()
	provider := "provider-a"
	external := "ext-1"
	round := "round-1"
	game := "game-1"
	ref := "ref-ext"
	before := int64(5000)
	after := int64(4000)
	version := int64(3)
	fcode := "INSUFFICIENT_FUNDS"

	tx := rehydrate(t, domain.RehydrateParams{
		Kind:                  domain.KindBet,
		Origin:                domain.OriginExternal,
		ProviderID:            &provider,
		ExternalTxID:          &external,
		PlayerID:              uuid.New(),
		WalletID:              uuid.New(),
		RoundID:               &round,
		GameID:                &game,
		Currency:              "BRL",
		AmountMinor:           1000,
		ReferenceExternalTxID: &ref,
		Status:                domain.StatusRejected,
		FailureCode:           &fcode,
		ResultDirection:       domain.DirectionDebit,
		ResultAmountMinor:     1000,
		BalanceBeforeMinor:    &before,
		BalanceAfterMinor:     &after,
		WalletVersionAfter:    &version,
		ReceivedVia:           domain.ViaSQS,
		CreatedAt:             time.Now(),
		UpdatedAt:             time.Now(),
	})

	out := transactionPayload(tx)
	assert.Equal(t, provider, out.ProviderID)
	assert.Equal(t, external, out.ExternalTransactionID)
	assert.Equal(t, round, out.RoundID)
	assert.Equal(t, game, out.GameID)
	assert.Equal(t, ref, out.ReferenceExternalTransactionID)
	assert.Equal(t, fcode, out.FailureCode)
	assert.Equal(t, string(domain.DirectionDebit), out.ResultDirection)
	require.NotNil(t, out.ResultAmount)
	assert.Equal(t, "10.00", out.ResultAmount.Amount)
	require.NotNil(t, out.BalanceBefore)
	assert.Equal(t, "50.00", out.BalanceBefore.Amount)
	require.NotNil(t, out.BalanceAfter)
	assert.Equal(t, "40.00", out.BalanceAfter.Amount)
	require.NotNil(t, out.WalletVersionAfter)
	assert.Equal(t, version, *out.WalletVersionAfter)
	assert.Equal(t, string(domain.ViaSQS), out.Channel)
}

func rehydrate(t *testing.T, p domain.RehydrateParams) *domain.WagerTransaction {
	t.Helper()
	tx, err := domain.RehydrateWagerTransaction(p)
	require.NoError(t, err)
	return tx
}
