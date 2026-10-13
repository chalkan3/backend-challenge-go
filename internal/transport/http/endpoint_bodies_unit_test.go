// White-box tests for the endpoint bodies themselves.
//
// endpoints.go is glue, but glue has a contract: the wallet read endpoints
// must pass their input through unchanged, the response must carry every
// mapped field (and omit a missing balance rather than print a zero), and the
// provider-scoped transaction endpoints must refuse a caller that is not the
// owner. The integration suite proves these endpoints against a real database;
// here they run against an in-memory TxManager, so the branches that need a
// database to reach — a not-found wallet, a forbidden provider — are pinned
// without one.
package http

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

func testServices(t *testing.T, store *fakeStore) (*service.WalletService, *service.WagerService) {
	t.Helper()
	return service.NewWalletService(store, testLogger(), store),
		service.NewWagerService(store, testLogger(), store)
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func uuidOf(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}

func unitProviderCtx(providerID string) context.Context {
	return context.WithValue(context.Background(), ctxPrincipal, &auth.Principal{
		ClientID: "provider-" + providerID, ProviderID: providerID,
	})
}

// submitCtx is the context a real submit arrives in: an internal identity plus
// the Idempotency-Key header the decoder would have placed on the context.
func submitCtx(key string) context.Context {
	return context.WithValue(internalTestCtx(), ctxIdempotencyKey, key)
}

func internalTestCtx() context.Context {
	return context.WithValue(context.Background(), ctxPrincipal, &auth.Principal{
		ClientID: "wallet-internal", Internal: true,
	})
}

// --- getWallet --------------------------------------------------------------

func TestGetWalletEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	ws, _ := testServices(t, store)
	ep := getWalletEndpoint(ws)

	t.Run("maps the wallet view", func(t *testing.T) {
		out, err := ep(context.Background(), getWalletRequest{WalletID: store.wallet.ID().String()})
		require.NoError(t, err)
		resp, ok := out.(walletResponse)
		require.True(t, ok)
		assert.Equal(t, store.wallet.ID().String(), resp.ID)
		assert.Equal(t, store.wallet.PlayerID().String(), resp.PlayerID)
		assert.Equal(t, "50.00", resp.Balance.Amount)
		assert.Equal(t, "BRL", resp.Balance.Currency)
		assert.Equal(t, int64(3), resp.Version)
	})

	t.Run("a malformed id is a validation error", func(t *testing.T) {
		_, err := ep(context.Background(), getWalletRequest{WalletID: "not-a-uuid"})
		require.Error(t, err)
	})

	t.Run("an unknown wallet is a not-found", func(t *testing.T) {
		_, err := ep(context.Background(), getWalletRequest{WalletID: uuidOf(t)})
		require.Error(t, err)
		var nf *service.NotFoundError
		require.ErrorAs(t, err, &nf)
	})
}

// --- listLedger -------------------------------------------------------------

func TestListLedgerEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	ws, _ := testServices(t, store)
	ep := listLedgerEndpoint(ws)

	out, err := ep(context.Background(), listLedgerRequest{WalletID: store.wallet.ID().String(), Limit: 10})
	require.NoError(t, err)
	resp, ok := out.(ledgerResponse)
	require.True(t, ok)
	require.Len(t, resp.Items, 1)
	e := resp.Items[0]
	assert.Equal(t, "CREDIT", e.Direction)
	assert.Equal(t, "10.00", e.Amount.Amount)
	assert.Equal(t, "40.00", e.BalanceBefore.Amount)
	assert.Equal(t, "50.00", e.BalanceAfter.Amount)
	assert.Empty(t, resp.NextCursor, "a full page with no more rows has no cursor")

	t.Run("ledger failure propagates", func(t *testing.T) {
		store.ledgerErr = assert.AnError
		_, err := ep(context.Background(), listLedgerRequest{WalletID: store.wallet.ID().String()})
		require.Error(t, err)
	})
}

// --- reconcile --------------------------------------------------------------

func TestReconcileEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	ws, _ := testServices(t, store)
	ep := reconcileEndpoint(ws)

	t.Run("consistent wallet", func(t *testing.T) {
		out, err := ep(context.Background(), reconcileRequest{WalletID: store.wallet.ID().String()})
		require.NoError(t, err)
		resp, ok := out.(reconciliationResponse)
		require.True(t, ok)
		assert.True(t, resp.Consistent)
		assert.Equal(t, "50.00", resp.StoredBalance.Amount)
		assert.Equal(t, "50.00", resp.CalculatedBalance.Amount)
		assert.Equal(t, "0.00", resp.Difference.Amount)
		assert.Equal(t, int64(1), resp.CheckedEntries)
	})

	t.Run("divergent wallet", func(t *testing.T) {
		store.reconcileFix = 700 // ledger sums to 57.00 while the wallet holds 50.00
		out, err := ep(context.Background(), reconcileRequest{WalletID: store.wallet.ID().String()})
		require.NoError(t, err)
		resp := out.(reconciliationResponse)
		assert.False(t, resp.Consistent)
		assert.Equal(t, "-7.00", resp.Difference.Amount)
		store.reconcileFix = 0
	})
}

// --- getTransaction / provider scoping --------------------------------------

func TestGetTransactionEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	_, wg := testServices(t, store)
	ep := getTransactionEndpoint(wg)
	tx := store.addTx("prov-1")

	t.Run("internal caller sees any transaction", func(t *testing.T) {
		out, err := ep(internalTestCtx(), getTransactionRequest{TransactionID: tx.ID().String()})
		require.NoError(t, err)
		resp := out.(transactionResponse)
		assert.Equal(t, tx.ID().String(), resp.TransactionID)
		assert.Equal(t, "BET", resp.Kind)
		assert.Equal(t, "prov-1", resp.ProviderID)
		assert.Equal(t, "10.00", resp.Amount.Amount)
		require.NotNil(t, resp.BalanceAfter)
		assert.Equal(t, "40.00", resp.BalanceAfter.Amount)
	})

	t.Run("the owning provider sees it", func(t *testing.T) {
		_, err := ep(unitProviderCtx("prov-1"), getTransactionRequest{TransactionID: tx.ID().String()})
		assert.NoError(t, err)
	})

	t.Run("another provider is forbidden", func(t *testing.T) {
		_, err := ep(unitProviderCtx("prov-2"), getTransactionRequest{TransactionID: tx.ID().String()})
		require.Error(t, err)
		he, ok := isHTTPError(err)
		require.True(t, ok)
		assert.Equal(t, 403, he.Status)
		assert.Equal(t, service.CodeForbidden, he.Code)
	})

	t.Run("unknown transaction is not found", func(t *testing.T) {
		_, err := ep(internalTestCtx(), getTransactionRequest{TransactionID: uuidOf(t)})
		require.Error(t, err)
		var nf *service.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.Equal(t, service.CodeTransactionNotFound, nf.Code)
	})
}

func TestGetProviderTransactionEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	_, wg := testServices(t, store)
	ep := getProviderTransactionEndpoint(wg)
	store.addTx("prov-1")

	req := getProviderTransactionRequest{ProviderID: "prov-1", ExternalTransactionID: "ext-1"}

	t.Run("the owning provider resolves by external id", func(t *testing.T) {
		out, err := ep(unitProviderCtx("prov-1"), req)
		require.NoError(t, err)
		resp := out.(transactionResponse)
		assert.Equal(t, "ext-1", resp.ExternalTransactionID)
		assert.Equal(t, "BET", resp.Kind)
	})

	t.Run("a different provider is forbidden before any lookup", func(t *testing.T) {
		_, err := ep(unitProviderCtx("prov-2"), req)
		require.Error(t, err)
		he, ok := isHTTPError(err)
		require.True(t, ok)
		assert.Equal(t, 403, he.Status)
	})

	t.Run("an internal caller is not scoped", func(t *testing.T) {
		_, err := ep(internalTestCtx(), req)
		assert.NoError(t, err)
	})

	t.Run("unknown external id is not found", func(t *testing.T) {
		_, err := ep(unitProviderCtx("prov-1"), getProviderTransactionRequest{ProviderID: "prov-1", ExternalTransactionID: "nope"})
		require.Error(t, err)
		var nf *service.NotFoundError
		require.ErrorAs(t, err, &nf)
	})
}

// --- openWallet -------------------------------------------------------------

// TestOpenWalletEndpoint covers the creation glue: the request fields reach the
// use case, the response echoes the freshly opened wallet and — the part worth
// pinning — the IdempotentReplay flag survives the trip, since clients rely on
// it to tell a first opening from a replay of the same key.
func TestOpenWalletEndpointUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	ws, _ := testServices(t, store)
	ep := openWalletEndpoint(ws)

	req := openWalletRequest{
		PlayerID:      uuidOf(t),
		InitialAmount: "25.00",
		Currency:      "BRL",
	}
	out, err := ep(context.Background(), req)
	require.NoError(t, err)
	resp, ok := out.(walletResponse)
	require.True(t, ok)
	assert.Equal(t, "25.00", resp.Balance.Amount)
	assert.Equal(t, "BRL", resp.Balance.Currency)
	assert.False(t, resp.IdempotentReplay)
	assert.NotEmpty(t, resp.ID)

	t.Run("a validation failure propagates", func(t *testing.T) {
		_, err := ep(context.Background(), openWalletRequest{
			PlayerID: "not-a-uuid", InitialAmount: "1.00", Currency: "BRL",
		})
		require.Error(t, err)
	})
}

// --- submit -----------------------------------------------------------------

// TestSubmitEndpointBuildsResponse runs a real BET through the submit
// endpoint: the wallet is debited, the transaction is persisted and the
// response builder maps every field of the result — including the nullable
// balances that only appear once the operation was processed. This is the one
// place the JSON contract of the write path is assembled, so it is pinned with
// a genuine WagerResult rather than a hand-built one.
func TestSubmitEndpointBuildsResponseUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	_, wg := testServices(t, store)
	ep := submitEndpoint(wg)

	req := submitRequest{
		ProviderID:                     "prov-1",
		ExternalTransactionID:          "ext-submit-1",
		PlayerID:                       store.wallet.PlayerID().String(),
		WalletID:                       store.wallet.ID().String(),
		Kind:                           "BET",
		Money:                          moneyWire{Amount: "10.00", Currency: "BRL"},
		ReferenceExternalTransactionID: "",
	}
	out, err := ep(submitCtx("idem-submit-1"), req)
	require.NoError(t, err)
	resp, ok := out.(submitResponse)
	require.True(t, ok)

	assert.NotEmpty(t, resp.TransactionID)
	assert.Equal(t, "PROCESSED", resp.Status)
	assert.Equal(t, "40.00", resp.Balance.Amount, "the snapshot reflects the debit")
	assert.False(t, resp.IdempotentReplay)
	assert.Empty(t, resp.FailureCode)
	assert.Nil(t, resp.NextRetryAt)

	assert.Len(t, store.entries, 2, "one opening entry plus the debit just applied")
	assert.NotEmpty(t, store.outbox, "a processed operation publishes its events")
}

// TestSubmitEndpointRejectsInsufficientFunds pins the business-rejection path
// through the endpoint: a BET larger than the balance is refused with a
// persisted REJECTED result and the failure fields populated, not an error.
func TestSubmitEndpointRejectsInsufficientFundsUnit(t *testing.T) {
	t.Parallel()
	store := newFakeStore(t)
	_, wg := testServices(t, store)
	ep := submitEndpoint(wg)

	req := submitRequest{
		ProviderID:            "prov-1",
		ExternalTransactionID: "ext-too-big",
		PlayerID:              store.wallet.PlayerID().String(),
		WalletID:              store.wallet.ID().String(),
		Kind:                  "BET",
		Money:                 moneyWire{Amount: "999.00", Currency: "BRL"},
	}
	out, err := ep(submitCtx("idem-too-big"), req)
	require.NoError(t, err, "a business rejection is a normal result, not a transport error")
	resp := out.(submitResponse)
	assert.Equal(t, "REJECTED", resp.Status)
	assert.Equal(t, service.FailInsufficientFunds, resp.FailureCode)
	assert.NotEmpty(t, resp.FailureMessage)
}
