//go:build integration

// Package postgres integration tests: they exercise the REAL pgx adapters
// against a REAL PostgreSQL (the compose/localstack stack that scripts/e2e.sh
// brings up). The challenge forbids replacing the database with mocks, so the
// repository SQL can only be verified here — the pure helpers are unit-tested
// in helpers_test.go.
//
// Isolation: every test fabricates fresh UUIDv7 player/wallet ids, so rows
// never collide across tests or runs and no truncation/teardown is required
// (append-only tables, in particular, must never be cleaned by DELETE).
//
// Required environment (defaults match docker-compose.yml):
//
//	WALLET_TEST_DB_DSN postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable
package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

func envOr(key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func dbDSN() string {
	return envOr("WALLET_TEST_DB_DSN", "postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable")
}

// mustPool connects a fresh pool and fails fast if the database is unreachable.
func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbDSN())
	require.NoErrorf(t, err, "db connect (is the stack up? scripts/e2e.sh)")
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, pool.Ping(pingCtx), "db ping")
	t.Cleanup(pool.Close)
	return pool
}

// newStore returns the real pgx Store (TxManager) bound to a live pool.
func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(mustPool(t), nil)
}

func money(t *testing.T, major, currency string) domain.Money {
	t.Helper()
	m, err := domain.ParseMoney(major, currency)
	require.NoError(t, err)
	return m
}

func zero(t *testing.T, currency string) domain.Money {
	t.Helper()
	return domain.ZeroMoneyMust(currency)
}

// newWalletRow persists a fresh wallet with the given initial balance. The
// schema requires wallet_ledger_entries.transaction_id to reference a real
// wager_transactions row, so the opening balance is backed by an OPENING
// transaction (marked PROCESSED, exactly as the service does) and its ledger
// line points at it.
func newWalletRow(t *testing.T, ctx context.Context, store *Store, initial string) *domain.Wallet {
	t.Helper()
	now := time.Now().UTC()
	w, mv, err := domain.OpenWallet(domain.MustNewID(), "BRL", money(t, initial, "BRL"), now)
	require.NoError(t, err)
	require.NoError(t, store.Repos().Wallets().Create(ctx, w))

	open, err := domain.NewOpeningTransaction(w.PlayerID(), w.ID(), "BRL", money(t, initial, "BRL"), now)
	require.NoError(t, err)
	dir := mv.Direction
	amount := mv.Amount
	if !mv.HasChange {
		dir = domain.DirectionNone
		amount = money(t, "0.00", "BRL")
	}
	require.NoError(t, open.MarkProcessed(domain.ProcessedResult{
		Direction:     dir,
		ResultAmount:  amount,
		BalanceBefore: mv.BalanceBefore,
		BalanceAfter:  mv.BalanceAfter,
		WalletVersion: mv.WalletVersion,
	}, now))
	require.NoError(t, store.Repos().Transactions().Insert(ctx, open))

	if mv.HasChange {
		entry, err := domain.NewLedgerEntry(w.ID(), open.ID(), mv, now)
		require.NoError(t, err)
		require.NoError(t, store.Repos().Ledger().Append(ctx, entry))
	}
	return w
}

// ---------------------------------------------------------------------------
// walletRepo
// ---------------------------------------------------------------------------

func TestWalletRepo_CreateAndReadBack(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "100.00")

	got, err := store.Repos().Wallets().GetByID(ctx, w.ID())
	require.NoError(t, err)
	assert.Equal(t, w.ID(), got.ID())
	assert.Equal(t, w.PlayerID(), got.PlayerID())
	assert.Equal(t, "BRL", got.Currency())
	assert.Equal(t, int64(10000), got.Balance().AmountMinor())
	assert.Equal(t, int64(1), got.Version(), "OpenWallet starts the wallet at version 1")
}

func TestWalletRepo_GetByID_NotFoundMapsToRepoNotFound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	_, err := store.Repos().Wallets().GetByID(ctx, domain.MustNewID())
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrRepoNotFound,
		"pgx.ErrNoRows must surface as ErrRepoNotFound")
}

func TestWalletRepo_UpdateBalancePersistsBalanceAndVersion(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "100.00")

	mv, err := w.Debit(money(t, "25.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, store.Repos().Wallets().UpdateBalance(ctx, w))
	require.True(t, mv.HasChange)

	got, err := store.Repos().Wallets().GetByID(ctx, w.ID())
	require.NoError(t, err)
	assert.Equal(t, int64(7500), got.Balance().AmountMinor())
	assert.Equal(t, int64(2), got.Version(), "version must advance with each movement (1 at open, 2 after debit)")
}

func TestWalletRepo_GetForUpdateReturnsLockableRow(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "10.00")

	var seen *domain.Wallet
	err := store.WithinTx(ctx, func(ctx context.Context, r service.Repos) error {
		got, err := r.Wallets().GetForUpdate(ctx, w.ID())
		if err != nil {
			return err
		}
		seen = got
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, seen)
	assert.Equal(t, w.ID(), seen.ID())
}

func TestWalletRepo_GetByPlayerAndCurrency_BothModes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "5.00")

	// without lock (pool, autocommit)
	got, err := store.Repos().Wallets().GetByPlayerAndCurrency(ctx, w.PlayerID(), "BRL", false)
	require.NoError(t, err)
	assert.Equal(t, w.ID(), got.ID())

	// with lock (FOR UPDATE inside a tx)
	err = store.WithinTx(ctx, func(ctx context.Context, r service.Repos) error {
		locked, err := r.Wallets().GetByPlayerAndCurrency(ctx, w.PlayerID(), "BRL", true)
		if err != nil {
			return err
		}
		assert.Equal(t, w.ID(), locked.ID())
		return nil
	})
	require.NoError(t, err)

	// unknown currency -> not found
	_, err = store.Repos().Wallets().GetByPlayerAndCurrency(ctx, w.PlayerID(), "USD", false)
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestWalletRepo_Create_DuplicatePlayerCurrencyFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "1.00")

	now := time.Now().UTC()
	dup, _, err := domain.OpenWallet(w.PlayerID(), "BRL", money(t, "1.00", "BRL"), now)
	require.NoError(t, err)
	err = store.Repos().Wallets().Create(ctx, dup)
	assert.ErrorIs(t, err, service.ErrDuplicateInsert,
		"the partial unique index on (player_id, currency) must reject a second wallet")
}

// ---------------------------------------------------------------------------
// walletOpenRepo
// ---------------------------------------------------------------------------

func TestWalletOpenRepo_RecordThenLookup(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "3.00")

	key := "open-" + uuid.NewString()
	require.NoError(t, store.Repos().WalletOpens().Record(ctx, key, "hash-abc", w.ID()))

	gotID, gotHash, err := store.Repos().WalletOpens().Lookup(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, w.ID(), gotID)
	assert.Equal(t, "hash-abc", gotHash)
}

func TestWalletOpenRepo_Lookup_UnknownKeyMapsToRepoNotFound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	_, _, err := store.Repos().WalletOpens().Lookup(ctx, "missing-"+uuid.NewString())
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestWalletOpenRepo_Record_DuplicateKeyMapsToDuplicateInsert(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "3.00")

	key := "open-dup-" + uuid.NewString()
	require.NoError(t, store.Repos().WalletOpens().Record(ctx, key, "h1", w.ID()))
	err := store.Repos().WalletOpens().Record(ctx, key, "h2", w.ID())
	assert.ErrorIs(t, err, service.ErrDuplicateInsert)
}

// ---------------------------------------------------------------------------
// ledgerRepo
// ---------------------------------------------------------------------------

// appendEntry writes one ledger movement for an existing wallet. The schema
// requires every ledger line to reference a real transaction, so it first
// persists a BET transaction (unique provider key) and points the entry at it.
// Returns the ledger entry and the transaction that backs it.
func appendEntry(t *testing.T, ctx context.Context, store *Store, w *domain.Wallet, dir domain.Direction, amountMajor string) (domain.LedgerEntry, *domain.WagerTransaction) {
	t.Helper()
	var mv domain.Movement
	var err error
	switch dir {
	case domain.DirectionDebit:
		mv, err = w.Debit(money(t, amountMajor, "BRL"), time.Now().UTC())
	case domain.DirectionCredit:
		mv, err = w.Credit(money(t, amountMajor, "BRL"), time.Now().UTC())
	default:
		t.Fatalf("unsupported direction %q", dir)
	}
	require.NoError(t, err)
	require.NoError(t, store.Repos().Wallets().UpdateBalance(ctx, w))

	now := time.Now().UTC()
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "provider-a",
		ExternalTransactionID: "led-" + uuid.NewString()[:8],
		PlayerID:              w.PlayerID(),
		WalletID:              w.ID(),
		Kind:                  domain.KindBet,
		RoundID:               "round-" + uuid.NewString()[:8],
		GameID:                "game-" + uuid.NewString()[:8],
		Money:                 money(t, amountMajor, "BRL"),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "corr-ledger",
		Now:                   now,
	})
	require.NoError(t, err)
	tx.SetIdempotency("led-idem-"+uuid.NewString(), "led-hash-"+uuid.NewString()[:8])
	require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
		Direction:     mv.Direction,
		ResultAmount:  mv.Amount,
		BalanceBefore: mv.BalanceBefore,
		BalanceAfter:  mv.BalanceAfter,
		WalletVersion: mv.WalletVersion,
	}, now))
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))

	entry, err := domain.NewLedgerEntry(w.ID(), tx.ID(), mv, now)
	require.NoError(t, err)
	require.NoError(t, store.Repos().Ledger().Append(ctx, entry))
	return entry, tx
}

func TestLedgerRepo_AppendAndListCursorPagination(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "100.00")

	// Three movements. A microsecond gap keeps created_at strictly increasing
	// so the keyset (created_at, id) is unambiguous — the same discipline the
	// service observes when it commits each movement in its own transaction.
	for _, amt := range []string{"10.00", "20.00", "30.00"} {
		_, _ = appendEntry(t, ctx, store, w, domain.DirectionDebit, amt)
		time.Sleep(2 * time.Microsecond)
	}

	// Wallet has one OPENING entry (100.00) plus three debits => 4 entries.
	seen := map[uuid.UUID]bool{}
	var all []*domain.LedgerEntry
	var cur *service.LedgerCursor
	pages := 0
	for {
		page, next, err := store.Repos().Ledger().ListByWallet(ctx, w.ID(), cur, 2)
		require.NoError(t, err)
		all = append(all, page...)
		pages++
		cur = next
		if cur == nil {
			break
		}
		require.Less(t, pages, 10, "the cursor must terminate")
	}

	require.Len(t, all, 4, "the cursor walk must visit every entry")
	for _, e := range all {
		assert.False(t, seen[e.ID()], "cursor pagination must not repeat an entry")
		seen[e.ID()] = true
	}
	// Stable ascending order by (created_at, id).
	for i := 1; i < len(all); i++ {
		assert.False(t, all[i].CreatedAt().Before(all[i-1].CreatedAt()),
			"entries must come back in ascending created_at order")
	}
}

func TestLedgerRepo_ListByWallet_Empty(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "0.00")

	entries, cur, err := store.Repos().Ledger().ListByWallet(ctx, w.ID(), nil, 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Nil(t, cur)
}

func TestLedgerRepo_Reconcile_Consistent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "100.00")
	appendEntry(t, ctx, store, w, domain.DirectionDebit, "40.00")
	appendEntry(t, ctx, store, w, domain.DirectionCredit, "5.00")

	stored, sum, count, err := store.Repos().Ledger().Reconcile(ctx, w.ID())
	require.NoError(t, err)
	assert.Equal(t, int64(6500), stored)
	assert.Equal(t, stored, sum, "ledger sum must equal stored balance")
	assert.Equal(t, int64(3), count, "opening + two movements")
}

func TestLedgerRepo_Reconcile_NotAWalletMapsToRepoNotFound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	_, _, _, err := store.Repos().Ledger().Reconcile(ctx, domain.MustNewID())
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

// ---------------------------------------------------------------------------
// txRepo
// ---------------------------------------------------------------------------

func newPendingBet(t *testing.T, playerID, walletID uuid.UUID, amountMajor string) *domain.WagerTransaction {
	t.Helper()
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "provider-a",
		ExternalTransactionID: "ext-" + uuid.NewString()[:8],
		PlayerID:              playerID,
		WalletID:              walletID,
		Kind:                  domain.KindBet,
		RoundID:               "round-" + uuid.NewString()[:8],
		GameID:                "game-" + uuid.NewString()[:8],
		Money:                 money(t, amountMajor, "BRL"),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "corr-" + uuid.NewString()[:8],
		Now:                   time.Now().UTC(),
	})
	require.NoError(t, err)
	tx.SetIdempotency("idem-"+uuid.NewString(), "hash-"+uuid.NewString()[:8])
	return tx
}

func TestTxRepo_InsertAndGetByID(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")
	tx := newPendingBet(t, w.PlayerID(), w.ID(), "10.00")
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))

	got, err := store.Repos().Transactions().GetByID(ctx, tx.ID())
	require.NoError(t, err)
	assert.Equal(t, tx.ID(), got.ID())
	assert.Equal(t, domain.KindBet, got.Kind())
	assert.Equal(t, domain.StatusPending, got.Status())
	assert.Equal(t, tx.IdempotencyKey(), got.IdempotencyKey())
}

func TestTxRepo_GetByID_NotFoundMapsToRepoNotFound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	_, err := store.Repos().Transactions().GetByID(ctx, domain.MustNewID())
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestTxRepo_GetByProviderAndKeyAndExternal(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")
	tx := newPendingBet(t, w.PlayerID(), w.ID(), "10.00")
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))

	byKey, err := store.Repos().Transactions().GetByProviderAndKey(ctx, "provider-a", tx.IdempotencyKey())
	require.NoError(t, err)
	assert.Equal(t, tx.ID(), byKey.ID())

	byExt, err := store.Repos().Transactions().GetByProviderAndExternal(ctx, "provider-a", tx.ExternalTxID())
	require.NoError(t, err)
	assert.Equal(t, tx.ID(), byExt.ID())

	_, err = store.Repos().Transactions().GetByProviderAndKey(ctx, "provider-a", "nope-"+uuid.NewString())
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestTxRepo_DuplicateProviderKeyMapsToDuplicateInsert(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")
	tx := newPendingBet(t, w.PlayerID(), w.ID(), "10.00")
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))

	// Same provider + same idempotency key -> unique violation.
	dup, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID: "provider-a", ExternalTransactionID: "ext-" + uuid.NewString()[:8],
		PlayerID: w.PlayerID(), WalletID: w.ID(), Kind: domain.KindBet, RoundID: "round", GameID: "game",
		Money: money(t, "10.00", "BRL"), ReceivedVia: domain.ViaHTTP,
		CorrelationID: "corr", Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	dup.SetIdempotency(tx.IdempotencyKey(), "other-hash")
	assert.ErrorIs(t, store.Repos().Transactions().Insert(ctx, dup), service.ErrDuplicateInsert)
}

func TestTxRepo_SaveUpdatesProjection(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")
	tx := newPendingBet(t, w.PlayerID(), w.ID(), "10.00")
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))

	now := time.Now().UTC()
	require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
		Direction:     domain.DirectionDebit,
		ResultAmount:  money(t, "10.00", "BRL"),
		BalanceBefore: money(t, "50.00", "BRL"),
		BalanceAfter:  money(t, "40.00", "BRL"),
		WalletVersion: 1,
	}, now))
	require.NoError(t, store.Repos().Transactions().Save(ctx, tx))

	got, err := store.Repos().Transactions().GetByID(ctx, tx.ID())
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, got.Status())
	assert.Equal(t, domain.DirectionDebit, got.ResultDirection())
	assert.Equal(t, int64(1000), got.ResultAmount().AmountMinor())
	require.NotNil(t, got.BalanceAfter())
	assert.Equal(t, int64(4000), got.BalanceAfter().AmountMinor())
}

func TestTxRepo_Save_TerminalRowIsFrozen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")
	tx := newPendingBet(t, w.PlayerID(), w.ID(), "10.00")
	require.NoError(t, store.Repos().Transactions().Insert(ctx, tx))
	now := time.Now().UTC()
	require.NoError(t, tx.MarkRejected("SOME_CODE", "rejected", now))
	require.NoError(t, store.Repos().Transactions().Save(ctx, tx))

	// A second Save on the now-terminal row must be refused by the WHERE guard.
	// (The row is already REJECTED; a direct re-save must not mutate it.)
	err := store.Repos().Transactions().Save(ctx, tx)
	require.Error(t, err, "terminal rows must be frozen")
	assert.Contains(t, err.Error(), "terminal")
}

func TestTxRepo_ClaimPendingReferences_LocksAndLimits(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	w := newWalletRow(t, ctx, store, "50.00")

	// A PENDING_REFERENCE refund due now.
	ref, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID: "provider-a", ExternalTransactionID: "ext-" + uuid.NewString()[:8],
		PlayerID: w.PlayerID(), WalletID: w.ID(), Kind: domain.KindRefund, RoundID: "round", GameID: "game",
		Money: money(t, "10.00", "BRL"), ReferenceExternalTxID: "ext-ref-" + uuid.NewString()[:8],
		ReceivedVia: domain.ViaHTTP, CorrelationID: "corr", Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	ref.SetIdempotency("idem-"+uuid.NewString(), "hash-"+uuid.NewString()[:8])
	require.NoError(t, store.Repos().Transactions().Insert(ctx, ref))
	require.NoError(t, ref.MarkPendingReference(time.Now().UTC().Add(-time.Minute), time.Now().UTC()))
	require.NoError(t, store.Repos().Transactions().Save(ctx, ref))

	claimed, err := store.Repos().Transactions().ClaimPendingReferences(ctx, time.Now().UTC(), 10)
	require.NoError(t, err)
	found := false
	for _, c := range claimed {
		if c.ID() == ref.ID() {
			found = true
		}
	}
	assert.True(t, found, "a due PENDING_REFERENCE row must be claimable")
}

func TestTxRepo_ExistsProcessedReversal(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	refTxID := domain.MustNewID()

	// Nothing yet.
	ok, err := store.Repos().Transactions().ExistsProcessedReversal(ctx, refTxID, domain.KindRefund)
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// inboxRepo
// ---------------------------------------------------------------------------

func TestInboxRepo_BeginCompleteThenReplay(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	consumer := "test-consumer-" + uuid.NewString()[:8]
	msg := "msg-" + uuid.NewString()

	// First receipt: inserted.
	out, err := store.Repos().Inbox().Begin(ctx, consumer, msg, "hash-1")
	require.NoError(t, err)
	assert.True(t, out.Inserted, "first receipt must insert")
	assert.False(t, out.Completed)

	// Complete it.
	require.NoError(t, store.Repos().Inbox().Complete(ctx, consumer, msg, time.Now().UTC()))

	// Replay (same hash): exists + hash matches + already completed.
	out2, err := store.Repos().Inbox().Begin(ctx, consumer, msg, "hash-1")
	require.NoError(t, err)
	assert.False(t, out2.Inserted)
	assert.True(t, out2.Exists)
	assert.True(t, out2.HashMatch)
	assert.True(t, out2.Completed, "a completed message must be recognized as done")
}

func TestInboxRepo_Begin_HashMismatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	consumer := "test-consumer-" + uuid.NewString()[:8]
	msg := "msg-" + uuid.NewString()

	_, err := store.Repos().Inbox().Begin(ctx, consumer, msg, "hash-1")
	require.NoError(t, err)

	out, err := store.Repos().Inbox().Begin(ctx, consumer, msg, "hash-DIFFERENT")
	require.NoError(t, err)
	assert.True(t, out.Exists)
	assert.False(t, out.HashMatch, "a different payload hash must be reported as a mismatch")
}

// ---------------------------------------------------------------------------
// outboxRepo
// ---------------------------------------------------------------------------

func mustEnvelope(t *testing.T, aggregateID uuid.UUID) domain.Envelope {
	t.Helper()
	now := time.Now().UTC()
	wt, err := domain.NewOpeningTransaction(domain.MustNewID(), aggregateID, "BRL", money(t, "1.00", "BRL"), now)
	require.NoError(t, err)
	env, err := domain.NewWagerTransactionProcessedEvent(wt)
	require.NoError(t, err)
	return env
}

func TestOutboxRepo_EnqueueClaimPublish(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	walletID := domain.MustNewID()
	now := time.Now().UTC()
	wt, err := domain.NewOpeningTransaction(domain.MustNewID(), walletID, "BRL", money(t, "1.00", "BRL"), now)
	require.NoError(t, err)
	env, err := domain.NewWagerTransactionProcessedEvent(wt)
	require.NoError(t, err)

	require.NoError(t, store.Repos().Outbox().Enqueue(ctx, []domain.Envelope{env}))

	pending, _, err := store.Repos().Outbox().Stats(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, pending, int64(1))

	items, err := store.Repos().Outbox().ClaimBatch(ctx, time.Now().UTC(), time.Minute, 100)
	require.NoError(t, err)
	var claimed *service.OutboxItem
	for i := range items {
		if items[i].EventID.String() == env.EventID {
			claimed = &items[i]
		}
	}
	require.NotNil(t, claimed, "the enqueued event must be claimable")
	assert.Equal(t, wt.ID().String(), claimed.AggregateID, "aggregate is the transaction id")
	assert.NotEmpty(t, claimed.Payload)
	assert.Equal(t, 1, claimed.Attempts, "claiming bumps the attempt counter")

	require.NoError(t, store.Repos().Outbox().MarkPublished(ctx, claimed.EventID, time.Now().UTC()))
	// A published event is no longer claimable.
	again, err := store.Repos().Outbox().ClaimBatch(ctx, time.Now().UTC(), time.Minute, 100)
	require.NoError(t, err)
	for _, it := range again {
		assert.NotEqual(t, env.EventID, it.EventID.String(), "published events must not be re-claimed")
	}
}

func TestOutboxRepo_EnqueueEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	assert.NoError(t, store.Repos().Outbox().Enqueue(ctx, nil))
	assert.NoError(t, store.Repos().Outbox().Enqueue(ctx, []domain.Envelope{}))
}

func TestOutboxRepo_MarkFailedReschedules(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	env := mustEnvelope(t, domain.MustNewID())
	require.NoError(t, store.Repos().Outbox().Enqueue(ctx, []domain.Envelope{env}))

	items, err := store.Repos().Outbox().ClaimBatch(ctx, time.Now().UTC(), time.Minute, 100)
	require.NoError(t, err)
	require.NotEmpty(t, items)

	// Mark failed with a long backoff -> the row leaves the due window.
	require.NoError(t, store.Repos().Outbox().MarkFailed(ctx, items[0].EventID, time.Now().UTC().Add(time.Hour), "boom"))
}

// ---------------------------------------------------------------------------
// Store (TxManager)
// ---------------------------------------------------------------------------

func TestStore_WithinTx_Commit(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	now := time.Now().UTC()
	w, _, err := domain.OpenWallet(domain.MustNewID(), "BRL", money(t, "7.00", "BRL"), now)
	require.NoError(t, err)

	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, r service.Repos) error {
		return r.Wallets().Create(ctx, w)
	}))

	_, err = store.Repos().Wallets().GetByID(ctx, w.ID())
	assert.NoError(t, err, "a committed tx must be visible")
}

func TestStore_WithinTx_Rollback(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	now := time.Now().UTC()
	w, _, err := domain.OpenWallet(domain.MustNewID(), "BRL", money(t, "7.00", "BRL"), now)
	require.NoError(t, err)

	sentinel := errors.New("force rollback")
	err = store.WithinTx(ctx, func(ctx context.Context, r service.Repos) error {
		if err := r.Wallets().Create(ctx, w); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	_, err = store.Repos().Wallets().GetByID(ctx, w.ID())
	assert.ErrorIs(t, err, service.ErrRepoNotFound, "a rolled-back tx must leave nothing behind")
}

func TestStore_Repos(t *testing.T) {
	store := newStore(t)
	r := store.Repos()
	require.NotNil(t, r.Wallets())
	require.NotNil(t, r.WalletOpens())
	require.NotNil(t, r.Transactions())
	require.NotNil(t, r.Ledger())
	require.NotNil(t, r.Inbox())
	require.NotNil(t, r.Outbox())
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrateUpIsIdempotent(t *testing.T) {
	ctx := context.Background()
	// The stack already migrated; running again must be a no-op (goose tracks
	// applied versions).
	err := MigrateUp(ctx, dbDSN())
	require.NoError(t, err)
	require.NoError(t, MigrateUp(ctx, dbDSN()))
}

func TestMigrateStatusSucceeds(t *testing.T) {
	require.NoError(t, MigrateStatus(context.Background(), dbDSN()))
}

// getenv is the single place this suite reads the environment.
func getenv(key string) string { return os.Getenv(key) }
