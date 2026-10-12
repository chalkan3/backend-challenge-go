// The http package tests pin the wire contract of the go-kit decoders and
// the error-code mapping used by the endpoint instrumentation. They are
// white-box because the codecs are unexported transport details.
package http

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

func decodeReq(t *testing.T, ctx context.Context, body string, headers map[string]string) (interface{}, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/wagering/transactions", bytes.NewBufferString(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return decodeSubmit(ctx, r)
}

func TestDecodeSubmit(t *testing.T) {
	ctx := context.Background()

	t.Run("valid body binds every business field", func(t *testing.T) {
		req, err := decodeReq(t, ctx, `{
			"providerId": "provider-a",
			"externalTransactionId": "tx-1",
			"playerId": "p",
			"walletId": "w",
			"kind": "BET",
			"money": {"amount": "25.00", "currency": "BRL"},
			"roundId": "r1",
			"gameId": "g1"
		}`, map[string]string{"Idempotency-Key": "key-1"})
		require.NoError(t, err)

		sub := req.(submitRequest)
		assert.Equal(t, "provider-a", sub.ProviderID)
		assert.Equal(t, "tx-1", sub.ExternalTransactionID)
		assert.Equal(t, "BET", sub.Kind)
		assert.Equal(t, "25.00", sub.Money.Amount)
		assert.Equal(t, "BRL", sub.Money.Currency)
		assert.Equal(t, "r1", sub.RoundID)
	})

	t.Run("idempotency key is mandatory", func(t *testing.T) {
		_, err := decodeReq(t, ctx, `{"providerId":"p","externalTransactionId":"e","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}`, nil)
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})

	t.Run("malformed json is a validation error", func(t *testing.T) {
		_, err := decodeReq(t, ctx, `{not-json`, map[string]string{"Idempotency-Key": "k"})
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})

	t.Run("OPENING is internal-only and never decodable", func(t *testing.T) {
		_, err := decodeReq(t, ctx, `{"providerId":"p","externalTransactionId":"e","kind":"OPENING","money":{"amount":"1.00","currency":"BRL"}}`,
			map[string]string{"Idempotency-Key": "k"})
		require.Error(t, err)
		assert.True(t, isValidationErr(err))
	})
}

func TestErrorCodeBuckets(t *testing.T) {
	assert.Empty(t, errorCode(nil))
	assert.Equal(t, "VALIDATION_FAILED", errorCode(&validationError{Detail: "x"}))
	assert.Equal(t, "WALLET_NOT_FOUND",
		errorCode(&service.NotFoundError{Code: service.CodeWalletNotFound, Detail: "w"}))
	assert.Equal(t, "IDEMPOTENCY_KEY_CONFLICT",
		errorCode(&service.ConflictError{Code: service.CodeIdemKeyConflict, Detail: "c"}))
	assert.Equal(t, "TRANSIENT_UNAVAILABLE", errorCode(&service.UnavailableError{Cause: assert.AnError}))
	assert.Equal(t, "UNAUTHORIZED", errorCode(errUnauthorized("no identity")))
	assert.Equal(t, "INTERNAL_ERROR", errorCode(assert.AnError))
}
