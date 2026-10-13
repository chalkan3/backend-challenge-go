//go:build integration

// The endpoints are covered twice: end-to-end by the integration suite (which
// drives a real app over HTTP) and here in-process, which is the only way the
// coverage-instrumented binary ever executes the endpoint bodies — in the
// compose topology they run inside the app container, not the test process.
//
// This file builds the real use cases over the real database and calls every
// endpoint function directly, with the identity injected into the context, so
// both the happy path and the authorization/validation branches are exercised.
package http

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// --- harness ----------------------------------------------------------------

func endpointPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WALLET_TEST_DB_DSN")
	if dsn == "" {
		dsn = "postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))
	t.Cleanup(pool.Close)
	return pool
}

type silentHook struct{}

func (silentHook) WagerOutcome(string, string, string, string) {}
func (silentHook) Duplicate(string)                            {}
func (silentHook) ReferenceOutcome(string)                     {}
func (silentHook) ReconciliationDivergence()                   {}
func (silentHook) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, silentEndpointSpan{}
}

type silentEndpointSpan struct{}

func (silentEndpointSpan) SetAttributes(...service.SpanAttr) {}
func (silentEndpointSpan) SetOutcome(string, string)         {}
func (silentEndpointSpan) RecordError(error)                 {}
func (silentEndpointSpan) End()                              {}

var _ service.Instrumentation = silentHook{}

// endpointHarness gives the endpoints their real dependencies over the test DB.
type endpointHarness struct {
	eps Endpoints
	m   *metrics.Metrics
	ws  *service.WalletService
	wg  *service.WagerService
}

func newEndpointHarness(t *testing.T) *endpointHarness {
	t.Helper()
	store := postgres.NewStore(endpointPool(t))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hook := silentHook{}
	ws := service.NewWalletService(store, log, hook)
	wg := service.NewWagerService(store, log, hook)
	m := metrics.New()
	return &endpointHarness{eps: NewEndpoints(ws, wg, m), m: m, ws: ws, wg: wg}
}

// internalCtx carries an internal service identity (wallet operations).
func internalCtx() context.Context {
	return context.WithValue(context.Background(), ctxPrincipal, &auth.Principal{
		ClientID: "wallet-internal", Internal: true,
	})
}

// providerCtx carries a provider identity bound to provider-a.
func providerCtx(provider string) context.Context {
	return context.WithValue(context.Background(), ctxPrincipal, &auth.Principal{
		ClientID: provider, ProviderID: provider,
	})
}

func newPlayer() string { return uuid.NewString() }

func (h *endpointHarness) open(t *testing.T, player string) string {
	t.Helper()
	res, err := h.eps.OpenWallet(internalCtx(), openWalletRequest{
		PlayerID:       player,
		InitialAmount:  "100.00",
		Currency:       "BRL",
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.NoError(t, err)
	return res.(walletResponse).ID
}

// --- endpoint bodies --------------------------------------------------------

func TestOpenWalletEndpoint(t *testing.T) {
	h := newEndpointHarness(t)
	res, err := h.eps.OpenWallet(internalCtx(), openWalletRequest{
		PlayerID: newPlayer(), InitialAmount: "50.00", Currency: "BRL",
		IdempotencyKey: "k-" + uuid.NewString(),
	})
	require.NoError(t, err)
	view := res.(walletResponse)
	assert.NotEmpty(t, view.ID)
	assert.Equal(t, "50.00", view.Balance.Amount)
	assert.False(t, view.IdempotentReplay)
}

func TestGetWalletEndpoint(t *testing.T) {
	h := newEndpointHarness(t)
	id := h.open(t, newPlayer())

	res, err := h.eps.GetWallet(internalCtx(), getWalletRequest{WalletID: id})
	require.NoError(t, err)
	assert.Equal(t, id, res.(walletResponse).ID)
}

func TestGetWalletEndpointBubblesNotFound(t *testing.T) {
	h := newEndpointHarness(t)
	_, err := h.eps.GetWallet(internalCtx(), getWalletRequest{WalletID: uuid.NewString()})
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
}

func TestGetWalletEndpointRejectsProviderIdentity(t *testing.T) {
	h := newEndpointHarness(t)
	// Wallet reads are internal-only: a provider identity is forbidden.
	_, err := h.eps.GetWallet(providerCtx("provider-a"), getWalletRequest{WalletID: uuid.NewString()})
	require.Error(t, err)
	he, ok := isHTTPError(err)
	require.True(t, ok)
	assert.Equal(t, 403, he.Status)
}

func TestEndpointRejectsMissingIdentity(t *testing.T) {
	h := newEndpointHarness(t)
	_, err := h.eps.OpenWallet(context.Background(), openWalletRequest{})
	require.Error(t, err)
	he, ok := isHTTPError(err)
	require.True(t, ok)
	assert.Equal(t, 401, he.Status)
}

func TestSubmitEndpointAcrossKinds(t *testing.T) {
	h := newEndpointHarness(t)
	player := newPlayer()
	walletID := h.open(t, player)

	submit := func(ctx context.Context, kind, amount, ext string) (interface{}, error) {
		return h.eps.SubmitTransaction(ctx, submitRequest{
			ProviderID:            "provider-a",
			ExternalTransactionID: ext,
			PlayerID:              player,
			WalletID:              walletID,
			Kind:                  kind,
			Money:                 moneyWire{Amount: amount, Currency: "BRL"},
			RoundID:               "r-" + ext,
			GameID:                "fortune-chimp",
		})
	}

	// BET debits.
	res, err := submit(context.WithValue(providerCtx("provider-a"), ctxIdempotencyKey, "k-"+uuid.NewString()), "BET", "30.00", "ext-"+uuid.NewString())
	require.NoError(t, err)
	bet := res.(submitResponse)
	assert.Equal(t, string(domain.StatusProcessed), bet.Status)
	assert.Equal(t, "70.00", bet.Balance.Amount)

	// WIN credits.
	res, err = submit(context.WithValue(providerCtx("provider-a"), ctxIdempotencyKey, "k-"+uuid.NewString()), "WIN", "10.00", "ext-"+uuid.NewString())
	require.NoError(t, err)
	assert.Equal(t, "80.00", res.(submitResponse).Balance.Amount)
}

func TestSubmitEndpointRejectsProviderMismatch(t *testing.T) {
	h := newEndpointHarness(t)
	player := newPlayer()
	walletID := h.open(t, player)

	// provider-b submitting while authenticated as provider-a must be refused
	// by the endpoint body (the providerId match is checked there).
	ctx := context.WithValue(providerCtx("provider-a"), ctxIdempotencyKey, "k-"+uuid.NewString())
	_, err := h.eps.SubmitTransaction(ctx, submitRequest{
		ProviderID: "provider-b", ExternalTransactionID: "ext-" + uuid.NewString(),
		PlayerID: player, WalletID: walletID, Kind: "BET",
		Money: moneyWire{Amount: "5.00", Currency: "BRL"},
	})
	require.Error(t, err)
	_, ok := isHTTPError(err)
	assert.True(t, ok, "a provider crossing identities must be a transport error")
}

func TestSubmitEndpointInsufficientFundsIsPersistedRejection(t *testing.T) {
	h := newEndpointHarness(t)
	player := newPlayer()
	walletID := h.open(t, player)

	ctx := context.WithValue(providerCtx("provider-a"), ctxIdempotencyKey, "k-"+uuid.NewString())
	res, err := h.eps.SubmitTransaction(ctx, submitRequest{
		ProviderID: "provider-a", ExternalTransactionID: "ext-" + uuid.NewString(),
		PlayerID: player, WalletID: walletID, Kind: "BET",
		Money:   moneyWire{Amount: "9999.00", Currency: "BRL"},
		RoundID: "r-" + uuid.NewString(), GameID: "fortune-chimp",
	})
	require.NoError(t, err, "a business rejection is a 200/422 body, not a transport error")
	assert.Equal(t, string(domain.StatusRejected), res.(submitResponse).Status)
	assert.Equal(t, service.FailInsufficientFunds, res.(submitResponse).FailureCode)
}

func TestListLedgerAndReconcileEndpoints(t *testing.T) {
	h := newEndpointHarness(t)
	player := newPlayer()
	walletID := h.open(t, player)

	res, err := h.eps.ListLedger(internalCtx(), listLedgerRequest{WalletID: walletID, Limit: 10})
	require.NoError(t, err)
	page := res.(ledgerResponse)
	assert.NotEmpty(t, page.Items, "the opening movement must be listed")

	res, err = h.eps.Reconcile(internalCtx(), reconcileRequest{WalletID: walletID})
	require.NoError(t, err)
	rep := res.(reconciliationResponse)
	assert.True(t, rep.Consistent)
}

func TestGetTransactionAndProviderTransactionEndpoints(t *testing.T) {
	h := newEndpointHarness(t)
	player := newPlayer()
	walletID := h.open(t, player)
	ext := "ext-" + uuid.NewString()

	ctx := context.WithValue(providerCtx("provider-a"), ctxIdempotencyKey, "k-"+uuid.NewString())
	res, err := h.eps.SubmitTransaction(ctx, submitRequest{
		ProviderID: "provider-a", ExternalTransactionID: ext,
		PlayerID: player, WalletID: walletID, Kind: "BET",
		Money:   moneyWire{Amount: "10.00", Currency: "BRL"},
		RoundID: "r-" + uuid.NewString(), GameID: "fortune-chimp",
	})
	require.NoError(t, err)
	txID := res.(submitResponse).TransactionID

	// Get by id, as the owning provider.
	res, err = h.eps.GetTransaction(providerCtx("provider-a"), getTransactionRequest{TransactionID: txID})
	require.NoError(t, err)
	assert.Equal(t, txID, res.(transactionResponse).TransactionID)

	// A different provider must not see it.
	_, err = h.eps.GetTransaction(providerCtx("provider-b"), getTransactionRequest{TransactionID: txID})
	require.Error(t, err)
	_, ok := isHTTPError(err)
	assert.True(t, ok)

	// Get by provider + external id.
	res, err = h.eps.GetProviderTransaction(providerCtx("provider-a"),
		getProviderTransactionRequest{ProviderID: "provider-a", ExternalTransactionID: ext})
	require.NoError(t, err)
	assert.Equal(t, txID, res.(transactionResponse).TransactionID)

	// Unknown external id → not found.
	_, err = h.eps.GetProviderTransaction(providerCtx("provider-a"),
		getProviderTransactionRequest{ProviderID: "provider-a", ExternalTransactionID: "missing-" + uuid.NewString()})
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
}
