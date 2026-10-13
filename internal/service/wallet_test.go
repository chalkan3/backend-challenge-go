package service_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Unit tests for the durable idempotency of POST /wallets. The fake store has
// no isolation, so these pin the use case's business rules; the SQL-level
// guarantee (key and wallet committing together) is exercised against real
// PostgreSQL in test/integration.

func newWalletService(store *fakeStore) *service.WalletService {
	return service.NewWalletService(store, slog.Default(), store.instrument())
}

func openInput(playerID, amount string) service.OpenWalletInput {
	return service.OpenWalletInput{PlayerID: playerID, InitialAmount: amount, Currency: "BRL"}
}

func TestOpenWithoutKeyCreatesWallet(t *testing.T) {
	store := newFakeStore(t, "0.00")
	playerID := uuid.NewString()

	view, err := newWalletService(store).Open(context.Background(), openInput(playerID, "100.00"))
	require.NoError(t, err)

	assert.False(t, view.IdempotentReplay)
	assert.Equal(t, "100.00", view.Balance.String())
	assert.Len(t, store.outbox, 2, "opening a wallet publishes two integration events")
}

func TestOpenReplaysSameKey(t *testing.T) {
	store := newFakeStore(t, "0.00")
	playerID := uuid.NewString()
	in := openInput(playerID, "100.00")
	in.IdempotencyKey = "open-key-1"
	svc := newWalletService(store)

	first, err := svc.Open(context.Background(), in)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)

	second, err := svc.Open(context.Background(), in)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.Equal(t, first.ID, second.ID, "the replay returns the original wallet, not a new one")
	assert.Equal(t, first.Balance.String(), second.Balance.String())
	assert.Len(t, store.outbox, 2, "no extra events for the replay")
}

func TestOpenIdempotentAcrossPayloadNormalization(t *testing.T) {
	store := newFakeStore(t, "0.00")
	playerID := uuid.NewString()
	key := "open-key-norm"
	svc := newWalletService(store)

	first := openInput(playerID, "100.00")
	first.IdempotencyKey = key
	_, err := svc.Open(context.Background(), first)
	require.NoError(t, err)

	// "100" and "100.00" are the same amount: the hash normalizes through
	// Money, so this is a replay, not a conflict.
	second := openInput(playerID, "100")
	second.IdempotencyKey = key
	view, err := svc.Open(context.Background(), second)
	require.NoError(t, err)

	assert.True(t, view.IdempotentReplay)
}

func TestOpenRejectsKeyReuseWithDifferentPayload(t *testing.T) {
	store := newFakeStore(t, "0.00")
	playerID := uuid.NewString()
	key := "open-key-conflict"
	svc := newWalletService(store)

	first := openInput(playerID, "100.00")
	first.IdempotencyKey = key
	_, err := svc.Open(context.Background(), first)
	require.NoError(t, err)

	second := openInput(playerID, "50.00")
	second.IdempotencyKey = key
	_, err = svc.Open(context.Background(), second)

	require.Error(t, err)
	assert.True(t, service.IsConflict(err))
	assert.Equal(t, service.CodeIdemKeyConflict, service.ConflictCode(err))
}

func TestOpenStillConflictsOnPlayerCurrencyWithoutKey(t *testing.T) {
	store := newFakeStore(t, "0.00")
	playerID := uuid.NewString()
	svc := newWalletService(store)

	_, err := svc.Open(context.Background(), openInput(playerID, "100.00"))
	require.NoError(t, err)

	// Without a key the uniqueness of (player, currency) is the guard: a
	// second opening of the same wallet is rejected, never duplicated.
	_, err = svc.Open(context.Background(), openInput(playerID, "100.00"))

	require.Error(t, err)
	assert.Equal(t, service.CodeWalletExists, service.ConflictCode(err))
}
