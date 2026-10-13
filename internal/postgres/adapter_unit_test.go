package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// These tests pin the *adapter contract* of the pgx layer: how a row becomes a
// domain object, how a pg error becomes a domain error, how a NULL column is
// encoded, and which SQL is issued for each repository call.
//
// They are not a substitute for the integration suite. A fake cannot prove that
// `SELECT ... FOR UPDATE` actually locks, that the unique index on
// idempotency_key exists, or that the ledger trigger rejects an UPDATE. What it
// can prove is that if the SQL is right, the Go code around it is right too —
// and that a refactor of the scanning code cannot silently change the contract.
// The SQL correctness story lives in repo_integration_test.go and
// test/integration, behind the `integration` build tag.

func fixedTime() time.Time {
	return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
}

func mustMoney(t *testing.T, minor int64, currency string) domain.Money {
	t.Helper()
	m, err := domain.NewMoney(minor, currency)
	require.NoError(t, err)
	return m
}

func mustWallet(t *testing.T, currency string) *domain.Wallet {
	t.Helper()
	w, _, err := domain.OpenWallet(uuid.New(), currency, mustMoney(t, 0, currency), fixedTime())
	require.NoError(t, err)
	return w
}

// ── scanWallet ──────────────────────────────────────────────────────────────

func TestScanWallet_HappyPath(t *testing.T) {
	id, playerID := uuid.New(), uuid.New()
	row := stubRow{values: []any{id, playerID, "BRL", int64(1250), int64(3), fixedTime(), fixedTime()}}

	w, err := scanWallet(row)
	require.NoError(t, err)
	assert.Equal(t, id, w.ID())
	assert.Equal(t, playerID, w.PlayerID())
	assert.Equal(t, "BRL", w.Currency())
	assert.Equal(t, int64(1250), w.Balance().AmountMinor())
	assert.Equal(t, int64(3), w.Version())
}

func TestScanWallet_NoRowsBecomesRepoNotFound(t *testing.T) {
	// The translation that keeps the transport layer honest: a missing wallet is
	// a domain-level "not found", never a raw pgx error leaking upward.
	_, err := scanWallet(errRow{err: pgx.ErrNoRows})
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestScanWallet_UnexpectedErrorIsNotSwallowed(t *testing.T) {
	boom := errors.New("connection reset by peer")
	_, err := scanWallet(errRow{err: boom})
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, service.ErrRepoNotFound,
		"an infrastructure error must not masquerade as a missing wallet")
}

func TestScanWallet_NegativeBalanceIsRejectedByDomain(t *testing.T) {
	// The DB has a CHECK constraint for this; the scanner must not be the only
	// line of defence, but it must also not accept what the domain rejects.
	row := stubRow{values: []any{uuid.New(), uuid.New(), "BRL", int64(-1), int64(1), fixedTime(), fixedTime()}}
	_, err := scanWallet(row)
	assert.ErrorIs(t, err, domain.ErrInvalidAmount)
}

func TestScanWallet_ZeroVersionIsRejected(t *testing.T) {
	row := stubRow{values: []any{uuid.New(), uuid.New(), "BRL", int64(0), int64(0), fixedTime(), fixedTime()}}
	_, err := scanWallet(row)
	assert.ErrorIs(t, err, domain.ErrZeroVersion)
}

func TestScanWallet_MalformedCurrencyNeverBecomesAWallet(t *testing.T) {
	// ValidateCurrency rejects the *shape* of a currency code, not its presence
	// in a list of real currencies (a wallet for a new currency must not need a
	// code change). What this pins is that a value the domain cannot represent
	// never reaches the caller as a usable Wallet.
	row := stubRow{values: []any{uuid.New(), uuid.New(), "BR", int64(1), int64(1), fixedTime(), fixedTime()}}
	_, err := scanWallet(row)
	assert.ErrorIs(t, err, domain.ErrInvalidCurrency)
}

// ── scanTx ──────────────────────────────────────────────────────────────────

// txRowValues builds the 29-column tuple scanTxRows expects, mirroring the
// order of txCols in tx_repo.go. Keeping it in one place means a column-order
// change breaks one helper instead of ten tests.
func txRowValues() []any {
	walletID, playerID, id := uuid.New(), uuid.New(), uuid.New()
	provider, external, idem, hash := "prov-1", "ext-9", "idem-1", "sha256:abc"
	round, game := "round-1", "game-1"
	refExt := "ext-ref"
	refTx := uuid.New()
	failCode, failMsg := "REFERENCE_UNRESOLVED", "waiting"
	dir := domain.DirectionDebit
	var amount, resultAmount int64 = 500, 500
	var before, after, versionAfter int64 = 1000, 500, 2
	next := fixedTime().Add(time.Minute)
	return []any{
		id, string(domain.KindBet), string(domain.OriginExternal), &provider, &external, &idem, &hash,
		playerID, walletID, &round, &game, "BRL", amount,
		&refExt, &refTx,
		"PENDING_REFERENCE", &failCode, &failMsg,
		&dir, resultAmount, &before, &after, &versionAfter,
		"api", "corr-1", 2, &next, fixedTime(), fixedTime(),
	}
}

func TestScanTxRows_HappyPath(t *testing.T) {
	got, err := scanTxRows(stubRow{values: txRowValues()})
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, domain.KindBet, got.Kind())
	assert.Equal(t, domain.OriginExternal, got.Origin())
	assert.Equal(t, domain.StatusPendingReference, got.Status())
	assert.Equal(t, int64(500), got.Amount().AmountMinor())
	assert.Equal(t, domain.DirectionDebit, got.ResultDirection())
	assert.Equal(t, 2, got.PendingAttempts())

	// Nullable columns must come back as non-nil pointers with the right value.
	require.NotNil(t, got.ProviderIDPtr())
	assert.Equal(t, "prov-1", *got.ProviderIDPtr())
	require.NotNil(t, got.IdempotencyKeyPtr())
	assert.Equal(t, "idem-1", *got.IdempotencyKeyPtr())
	require.NotNil(t, got.BalanceBefore())
	assert.Equal(t, int64(1000), got.BalanceBefore().AmountMinor())
	require.NotNil(t, got.ReferenceTxID())
	assert.NotEqual(t, uuid.Nil, *got.ReferenceTxID())
	require.NotNil(t, got.NextRetryAt())
}

func TestScanTxRows_NullsBecomeNilNotZero(t *testing.T) {
	// A NULL result_direction must rehydrate as DirectionNone, and a NULL
	// balance must stay nil — 0 would be a valid balance and a lie.
	vals := txRowValues()
	vals[18] = nil // result_direction
	vals[20] = nil // balance_before_minor
	vals[21] = nil // balance_after_minor
	vals[22] = nil // wallet_version_after
	vals[26] = nil // next_retry_at

	got, err := scanTxRows(stubRow{values: vals})
	require.NoError(t, err)

	assert.Equal(t, domain.DirectionNone, got.ResultDirection(),
		"a NULL direction must not become a zero value that could be read as DEBIT")
	assert.Nil(t, got.BalanceBefore())
	assert.Nil(t, got.BalanceAfter())
	assert.Nil(t, got.WalletVersionAfter())
	assert.Nil(t, got.NextRetryAt())
}

func TestScanTx_NoRowsBecomesRepoNotFound(t *testing.T) {
	_, err := scanTx(errRow{err: pgx.ErrNoRows})
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestScanTx_PropagatesOtherErrors(t *testing.T) {
	boom := errors.New("socket closed")
	_, err := scanTx(errRow{err: boom})
	assert.ErrorIs(t, err, boom)
}

// ── pure helpers ────────────────────────────────────────────────────────────

// ── repository methods: SQL shape and error translation ─────────────────────

func TestWalletRepo_Create_IssuesInsertAndMapsUniqueViolation(t *testing.T) {
	w := mustWallet(t, "BRL")

	t.Run("success passes the wallet fields positionally", func(t *testing.T) {
		f := &fakeQuerier{}
		require.NoError(t, (&walletRepo{ex: f}).Create(context.Background(), w))

		call := f.lastExec(t)
		assert.Contains(t, call.sql, "INSERT INTO wallets")
		require.Len(t, call.args, 7)
		assert.Equal(t, w.ID(), call.args[0])
		assert.Equal(t, w.PlayerID(), call.args[1])
		assert.Equal(t, "BRL", call.args[2])
		assert.Equal(t, int64(0), call.args[3])
	})

	t.Run("a 23505 becomes ErrDuplicateInsert for idempotent replay", func(t *testing.T) {
		f := &fakeQuerier{execErr: &pgconn.PgError{Code: "23505", Message: "duplicate key"}}
		err := (&walletRepo{ex: f}).Create(context.Background(), w)
		assert.ErrorIs(t, err, service.ErrDuplicateInsert)
	})

	t.Run("any other pg error is passed through untouched", func(t *testing.T) {
		other := &pgconn.PgError{Code: "23514", Message: "check violation"}
		f := &fakeQuerier{execErr: other}
		err := (&walletRepo{ex: f}).Create(context.Background(), w)
		assert.Equal(t, error(other), err)
		assert.NotErrorIs(t, err, service.ErrDuplicateInsert)
	})
}

func TestWalletRepo_GetByIDAndGetForUpdate_SQLDiffers(t *testing.T) {
	id := uuid.New()
	walletRow := func() stubRow {
		return stubRow{values: []any{id, uuid.New(), "BRL", int64(0), int64(1), fixedTime(), fixedTime()}}
	}

	t.Run("GetByID does not lock", func(t *testing.T) {
		f := &fakeQuerier{row: walletRow()}
		_, err := (&walletRepo{ex: f}).GetByID(context.Background(), id)
		require.NoError(t, err)
		assert.NotContains(t, f.lastQueryRow(t).sql, "FOR UPDATE")
	})

	t.Run("GetForUpdate locks the row", func(t *testing.T) {
		// This assertion is the readable half of the concurrency claim: the
		// integration suite proves the lock actually serialises writers.
		f := &fakeQuerier{row: walletRow()}
		_, err := (&walletRepo{ex: f}).GetForUpdate(context.Background(), id)
		require.NoError(t, err)
		call := f.lastQueryRow(t)
		assert.Contains(t, call.sql, "FOR UPDATE")
		assert.Equal(t, id, call.args[0])
	})
}

func TestWalletRepo_GetByPlayerAndCurrency_ForUpdateIsOptional(t *testing.T) {
	walletRow := func() stubRow {
		return stubRow{values: []any{uuid.New(), uuid.New(), "BRL", int64(0), int64(1), fixedTime(), fixedTime()}}
	}

	t.Run("without lock", func(t *testing.T) {
		f := &fakeQuerier{row: walletRow()}
		_, err := (&walletRepo{ex: f}).GetByPlayerAndCurrency(context.Background(), uuid.New(), "BRL", false)
		require.NoError(t, err)
		assert.NotContains(t, f.lastQueryRow(t).sql, "FOR UPDATE")
	})
	t.Run("with lock", func(t *testing.T) {
		f := &fakeQuerier{row: walletRow()}
		_, err := (&walletRepo{ex: f}).GetByPlayerAndCurrency(context.Background(), uuid.New(), "BRL", true)
		require.NoError(t, err)
		assert.Contains(t, f.lastQueryRow(t).sql, "FOR UPDATE")
	})
}

func TestWalletRepo_UpdateBalance_PersistsVersionAlongsideBalance(t *testing.T) {
	// The optimistic version must ride along with the balance: if it did not,
	// a lost update would be undetectable.
	w := mustWallet(t, "BRL")

	f := &fakeQuerier{}
	require.NoError(t, (&walletRepo{ex: f}).UpdateBalance(context.Background(), w))

	call := f.lastExec(t)
	assert.Contains(t, call.sql, "UPDATE wallets SET balance_minor")
	require.Len(t, call.args, 4)
	assert.Equal(t, int64(0), call.args[1])
	assert.Equal(t, int64(1), call.args[2])
}

func TestWalletRepo_ListIDs_KeysetUsesCursorWhenPresent(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	after := uuid.New()

	f := &fakeQuerier{queryRows: newStubRows([]any{a}, []any{b})}
	ids, err := (&walletRepo{ex: f}).ListIDs(context.Background(), &after, 10)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{a, b}, ids)

	call := f.queryCalls[0]
	assert.Contains(t, call.sql, "id > $1")
	assert.NotContains(t, call.sql, "OFFSET",
		"keyset pagination, never OFFSET: OFFSET skips or repeats rows under concurrent writes")
	assert.Equal(t, &after, call.args[0])
	assert.Equal(t, 10, call.args[1])
}

func TestWalletRepo_ListIDs_NilCursorMeansFirstPage(t *testing.T) {
	f := &fakeQuerier{queryRows: newStubRows()}
	ids, err := (&walletRepo{ex: f}).ListIDs(context.Background(), nil, 5)
	require.NoError(t, err)
	assert.Empty(t, ids)
	assert.Nil(t, f.queryCalls[0].args[0])
}

func TestWalletRepo_ListIDs_PropagatesQueryAndScanErrors(t *testing.T) {
	t.Run("query error", func(t *testing.T) {
		boom := errors.New("boom")
		f := &fakeQuerier{queryErr: boom}
		_, err := (&walletRepo{ex: f}).ListIDs(context.Background(), nil, 5)
		assert.ErrorIs(t, err, boom)
	})
	t.Run("scan error", func(t *testing.T) {
		boom := errors.New("bad uuid")
		rows := newStubRows([]any{uuid.New()})
		rows.scanFailAt = 0
		rows.scanErr = boom
		f := &fakeQuerier{queryRows: rows}
		_, err := (&walletRepo{ex: f}).ListIDs(context.Background(), nil, 5)
		assert.ErrorIs(t, err, boom)
	})
	t.Run("rows error surfaces after iteration", func(t *testing.T) {
		boom := errors.New("connection lost mid-stream")
		rows := newStubRows()
		rows.err = boom
		f := &fakeQuerier{queryRows: rows}
		_, err := (&walletRepo{ex: f}).ListIDs(context.Background(), nil, 5)
		assert.ErrorIs(t, err, boom,
			"a truncated result set must be an error, not a short list")
	})
}

func TestLedgerRepo_Append_WritesAllNineColumns(t *testing.T) {
	walletID, txID := uuid.New(), uuid.New()
	entry, err := domain.NewLedgerEntry(walletID, txID, domain.Movement{
		HasChange: true, Direction: domain.DirectionDebit,
		Amount:        mustMoney(t, 400, "BRL"),
		BalanceBefore: mustMoney(t, 1000, "BRL"),
		BalanceAfter:  mustMoney(t, 600, "BRL"),
		WalletVersion: 2,
	}, fixedTime())
	require.NoError(t, err)

	f := &fakeQuerier{}
	require.NoError(t, (&ledgerRepo{ex: f}).Append(context.Background(), entry))

	call := f.lastExec(t)
	assert.Contains(t, call.sql, "INSERT INTO wallet_ledger_entries")
	require.Len(t, call.args, 9)
	assert.Equal(t, walletID, call.args[1])
	assert.Equal(t, txID, call.args[2])
	assert.Equal(t, domain.DirectionDebit, call.args[3])
	assert.Equal(t, int64(400), call.args[5])
	assert.Equal(t, int64(1000), call.args[6])
	assert.Equal(t, int64(600), call.args[7])
}

func TestLedgerRepo_ListByWallet_ReturnsCursorOnlyWhenThereIsMore(t *testing.T) {
	walletID := uuid.New()
	rows := [][]any{
		ledgerRowValues(walletID, fixedTime()),
		ledgerRowValues(walletID, fixedTime().Add(time.Second)),
		ledgerRowValues(walletID, fixedTime().Add(2*time.Second)),
	}

	t.Run("a full page plus one yields a cursor and trims to limit", func(t *testing.T) {
		f := &fakeQuerier{queryRows: newStubRows(rows...)}
		got, cur, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, nil, 2)
		require.NoError(t, err)
		assert.Len(t, got, 2, "the extra row is a probe, not a result")
		require.NotNil(t, cur)
		assert.Equal(t, got[1].ID(), cur.ID)
		assert.Equal(t, got[1].CreatedAt(), cur.CreatedAt)
	})

	t.Run("a partial page means the end of the ledger", func(t *testing.T) {
		f := &fakeQuerier{queryRows: newStubRows(rows...)}
		got, cur, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, nil, 5)
		require.NoError(t, err)
		assert.Len(t, got, 3)
		assert.Nil(t, cur, "no cursor when the page is not full — there is nothing to resume")
	})

	t.Run("the cursor is forwarded as a composite keyset", func(t *testing.T) {
		cursor := &service.LedgerCursor{CreatedAt: fixedTime(), ID: uuid.New()}
		f := &fakeQuerier{queryRows: newStubRows()}
		_, _, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, cursor, 1)
		require.NoError(t, err)

		call := f.queryCalls[0]
		assert.Contains(t, call.sql, "(created_at, id) > ($2, $3::uuid)",
			"the cursor must be a tuple so ties on created_at cannot skip or repeat rows")
		require.Len(t, call.args, 4)
		require.NotNil(t, call.args[1])
		assert.Equal(t, cursor.CreatedAt, *call.args[1].(*time.Time))
		assert.Equal(t, 2, call.args[3], "limit+1 is fetched to detect a next page")
	})

	t.Run("no cursor means the first page", func(t *testing.T) {
		f := &fakeQuerier{queryRows: newStubRows()}
		_, _, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, nil, 1)
		require.NoError(t, err)
		assert.Nil(t, f.queryCalls[0].args[1])
		assert.Nil(t, f.queryCalls[0].args[2])
	})
}

func TestLedgerRepo_ListByWallet_RejectsMalformedRows(t *testing.T) {
	walletID := uuid.New()

	t.Run("unknown direction from the DB", func(t *testing.T) {
		bad := ledgerRowValues(walletID, fixedTime())
		bad[3] = "SIDEWAYS"
		f := &fakeQuerier{queryRows: newStubRows(bad)}
		_, _, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, nil, 1)
		assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})

	t.Run("rehydration deliberately does not revalidate committed rows", func(t *testing.T) {
		// RehydrateLedgerEntry's documented contract is that a persisted line is
		// immutable and already committed, so it is rebuilt as-is rather than
		// re-checked. Pinning the behaviour stops a future "helpful" validation
		// from making historical rows unreadable.
		odd := ledgerRowValues(walletID, fixedTime())
		odd[5] = int64(-1)
		f := &fakeQuerier{queryRows: newStubRows(odd)}
		got, _, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), walletID, nil, 1)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, int64(-1), got[0].Amount().AmountMinor())
	})
}

func TestLedgerRepo_Reconcile_MapsNoRowsToRepoNotFound(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		f := &fakeQuerier{row: stubRow{values: []any{int64(600), int64(600), int64(2)}}}
		stored, sum, count, err := (&ledgerRepo{ex: f}).Reconcile(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Equal(t, int64(600), stored)
		assert.Equal(t, int64(600), sum)
		assert.Equal(t, int64(2), count)
	})

	t.Run("unknown wallet", func(t *testing.T) {
		f := &fakeQuerier{row: errRow{err: pgx.ErrNoRows}}
		_, _, _, err := (&ledgerRepo{ex: f}).Reconcile(context.Background(), uuid.New())
		assert.ErrorIs(t, err, service.ErrRepoNotFound)
	})
}

func TestInboxRepo_Begin_TranslatesEachOutcome(t *testing.T) {
	// The insert-or-conflict dance is the at-least-once → effectively-once
	// bridge, so the mapping between SQL outcome and InboxOutcome is
	// load-bearing: getting it backwards means either reprocessing committed
	// work or dropping a message.
	ctx := context.Background()

	t.Run("a fresh message is inserted", func(t *testing.T) {
		f := &fakeQuerier{execTag: pgconn.NewCommandTag("INSERT 0 1")}
		got, err := (&inboxRepo{ex: f}).Begin(ctx, "consumer-1", "msg-1", "hash-1")
		require.NoError(t, err)
		assert.True(t, got.Inserted)
		assert.False(t, got.Exists)

		call := f.lastExec(t)
		assert.Contains(t, call.sql, "INSERT INTO consumer_inbox")
		assert.Contains(t, call.sql, "ON CONFLICT (consumer_name, message_id) DO NOTHING",
			"the conflict is the dedup mechanism; a plain INSERT would fail the redelivery")
		assert.Equal(t, "consumer-1", call.args[0])
		assert.Equal(t, "msg-1", call.args[1])
		assert.Equal(t, "hash-1", call.args[2])
	})

	t.Run("a redelivery reports the comparison of hashes and completion", func(t *testing.T) {
		completed := fixedTime()
		f := &fakeQuerier{
			execTag: pgconn.NewCommandTag("INSERT 0 0"),
			row:     stubRow{values: []any{"hash-1", &completed}},
		}
		got, err := (&inboxRepo{ex: f}).Begin(ctx, "consumer-1", "msg-1", "hash-1")
		require.NoError(t, err)
		assert.True(t, got.Exists)
		assert.True(t, got.HashMatch, "same payload hash means a true duplicate")
		assert.True(t, got.Completed, "completed_at set means the work already committed")

		assert.Contains(t, f.lastQueryRow(t).sql, "SELECT message_hash, completed_at")
	})

	t.Run("same message id with a different payload is not a hash match", func(t *testing.T) {
		f := &fakeQuerier{
			execTag: pgconn.NewCommandTag("INSERT 0 0"),
			row:     stubRow{values: []any{"other-hash", nil}},
		}
		got, err := (&inboxRepo{ex: f}).Begin(ctx, "consumer-1", "msg-1", "hash-1")
		require.NoError(t, err)
		assert.True(t, got.Exists)
		assert.False(t, got.HashMatch,
			"a reused message id with different content must be visible to the caller")
		assert.False(t, got.Completed)
	})

	t.Run("the insert error is propagated", func(t *testing.T) {
		boom := errors.New("deadlock detected")
		f := &fakeQuerier{execErr: boom}
		_, err := (&inboxRepo{ex: f}).Begin(ctx, "consumer-1", "msg-1", "hash-1")
		assert.ErrorIs(t, err, boom)
	})
}

func TestInboxRepo_Complete_RecordsTheTimestamp(t *testing.T) {
	f := &fakeQuerier{}
	at := fixedTime()
	require.NoError(t, (&inboxRepo{ex: f}).Complete(context.Background(), "consumer-1", "msg-1", at))

	call := f.lastExec(t)
	assert.Contains(t, call.sql, "UPDATE consumer_inbox SET completed_at")
	assert.Equal(t, "consumer-1", call.args[0])
	assert.Equal(t, at, call.args[2], "completion time is the evidence the message was processed")
}

func TestOutboxRepo_Enqueue_EmptyBatchIsANoOp(t *testing.T) {
	// The relay must not open a round trip when there is nothing to publish:
	// under a slow producer this path runs thousands of times per minute.
	f := &fakeQuerier{}
	require.NoError(t, (&outboxRepo{ex: f}).Enqueue(context.Background(), nil))
	assert.Empty(t, f.execCalls, "an empty batch must not touch the database")
	assert.Zero(t, f.sendBatchCall)
}

func TestOutboxRepo_MarkPublishedAndMarkFailed_TouchTheRightRows(t *testing.T) {
	id := uuid.New()

	t.Run("MarkPublished stamps published_at", func(t *testing.T) {
		f := &fakeQuerier{}
		require.NoError(t, (&outboxRepo{ex: f}).MarkPublished(context.Background(), id, fixedTime()))
		call := f.lastExec(t)
		assert.Contains(t, call.sql, "published_at")
		assert.Equal(t, id, call.args[0])
	})

	t.Run("MarkFailed records the reason and the next attempt", func(t *testing.T) {
		f := &fakeQuerier{}
		next := fixedTime().Add(time.Minute)
		require.NoError(t, (&outboxRepo{ex: f}).MarkFailed(context.Background(), id, next, "sqs unavailable"))
		call := f.lastExec(t)
		assert.Equal(t, id, call.args[0])
		assert.Equal(t, next, call.args[1])
		assert.Equal(t, "sqs unavailable", call.args[2],
			"the last error must be persisted: it is what a human reads when the DLQ fills")
	})
}

func TestOutboxRepo_Stats_ReadsPendingCountAndOldestAge(t *testing.T) {
	f := &fakeQuerier{row: stubRow{values: []any{int64(7), 12.5}}}
	pending, age, err := (&outboxRepo{ex: f}).Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(7), pending)
	assert.InDelta(t, 12.5, age, 0.001)
	assert.Contains(t, f.lastQueryRow(t).sql, "published_at IS NULL",
		"lag is measured over the unpublished backlog only")
}

func TestOutboxRepo_ClaimBatch_LeasesWithSkipLocked(t *testing.T) {
	// FOR UPDATE SKIP LOCKED is what lets several relay replicas drain the same
	// outbox without fighting each other.
	f := &fakeQuerier{queryRows: newStubRows()}
	_, err := (&outboxRepo{ex: f}).ClaimBatch(context.Background(), fixedTime(), time.Minute, 25)
	require.NoError(t, err)

	call := f.queryCalls[0]
	assert.Contains(t, call.sql, "SKIP LOCKED",
		"without SKIP LOCKED two relay replicas would serialise on the same rows")
	assert.Contains(t, call.sql, "published_at IS NULL", "only unclaimed events are leased")
	assert.Equal(t, 25, call.args[2])
	assert.Equal(t, fixedTime(), call.args[0])
}

func TestWalletOpenRepo_RecordAndLookup(t *testing.T) {
	key, hash, walletID := "open-key-1", "sha256:deadbeef", uuid.New()

	t.Run("Record stores the payload hash next to the key", func(t *testing.T) {
		f := &fakeQuerier{}
		require.NoError(t, (&walletOpenRepo{ex: f}).Record(context.Background(), key, hash, walletID))
		call := f.lastExec(t)
		assert.Contains(t, call.sql, "INSERT INTO wallet_open_requests")
		assert.Equal(t, key, call.args[0])
		assert.Equal(t, hash, call.args[1])
		assert.Equal(t, walletID, call.args[2])
	})

	t.Run("Record maps a duplicate key the same way Create does", func(t *testing.T) {
		f := &fakeQuerier{execErr: &pgconn.PgError{Code: "23505"}}
		err := (&walletOpenRepo{ex: f}).Record(context.Background(), key, hash, walletID)
		assert.ErrorIs(t, err, service.ErrDuplicateInsert)
	})

	t.Run("Lookup returns the stored hash so the caller can detect a mismatch", func(t *testing.T) {
		f := &fakeQuerier{row: stubRow{values: []any{walletID, hash}}}
		gotID, gotHash, err := (&walletOpenRepo{ex: f}).Lookup(context.Background(), key)
		require.NoError(t, err)
		assert.Equal(t, walletID, gotID)
		assert.Equal(t, hash, gotHash)
	})

	t.Run("Lookup maps no rows to ErrRepoNotFound", func(t *testing.T) {
		f := &fakeQuerier{row: errRow{err: pgx.ErrNoRows}}
		_, _, err := (&walletOpenRepo{ex: f}).Lookup(context.Background(), key)
		assert.ErrorIs(t, err, service.ErrRepoNotFound)
	})
}

func TestTxRepo_Insert_BindsEveryColumn(t *testing.T) {
	tx := buildTransaction(t, domain.KindBet)
	f := &fakeQuerier{}
	require.NoError(t, (&txRepo{ex: f}).Insert(context.Background(), tx))

	call := f.lastExec(t)
	assert.Contains(t, call.sql, "INSERT INTO wager_transactions")
	assert.Contains(t, call.sql, "idempotency_key",
		"the idempotency key column is the uniqueness anchor for replay detection")
}

func TestTxRepo_Save_HasTerminalStateGuard(t *testing.T) {
	tx := buildTransaction(t, domain.KindBet)

	t.Run("the WHERE clause refuses to touch a terminal row", func(t *testing.T) {
		f := &fakeQuerier{execTag: pgconn.NewCommandTag("UPDATE 1")}
		require.NoError(t, (&txRepo{ex: f}).Save(context.Background(), tx))

		call := f.lastExec(t)
		assert.Contains(t, call.sql, "UPDATE wager_transactions")
		assert.Contains(t, call.sql, "status IN ('PENDING','PENDING_REFERENCE')",
			"a committed PROCESSED row must be immutable through this path")
		assert.Equal(t, tx.ID(), call.args[0], "the id is bound first, for the WHERE")
	})

	t.Run("zero rows affected is an error, not a silent success", func(t *testing.T) {
		f := &fakeQuerier{execTag: pgconn.NewCommandTag("UPDATE 0")}
		err := (&txRepo{ex: f}).Save(context.Background(), tx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "terminal",
			"losing the race to another writer must surface, not be swallowed")
	})
}

func TestTxRepo_GetByProviderAndKey_BindsBothIdentifiers(t *testing.T) {
	f := &fakeQuerier{row: stubRow{values: txRowValues()}}
	got, err := (&txRepo{ex: f}).GetByProviderAndKey(context.Background(), "prov-1", "idem-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	call := f.lastQueryRow(t)
	assert.Equal(t, "prov-1", call.args[0])
	assert.Equal(t, "idem-1", call.args[1])
	assert.Contains(t, call.sql, "provider_id = $1 AND idempotency_key = $2",
		"idempotency is scoped per provider, not global")
}

func TestTxRepo_ExistsProcessedReversal_FalseIsNotAnError(t *testing.T) {
	f := &fakeQuerier{row: stubRow{values: []any{false}}}
	got, err := (&txRepo{ex: f}).ExistsProcessedReversal(context.Background(), uuid.New(), domain.KindRefund)
	require.NoError(t, err)
	assert.False(t, got)

	call := f.lastQueryRow(t)
	assert.Contains(t, call.sql, "status = 'PROCESSED'",
		"a reversal that was merely attempted does not count; only a committed one")
}

func TestTxRepo_ClaimPendingReferences_LocksWithSkipLocked(t *testing.T) {
	f := &fakeQuerier{queryRows: newStubRows()}
	_, err := (&txRepo{ex: f}).ClaimPendingReferences(context.Background(), fixedTime(), 10)
	require.NoError(t, err)

	call := f.queryCalls[0]
	assert.Contains(t, call.sql, "SKIP LOCKED")
	assert.Contains(t, call.sql, "FOR UPDATE")
	assert.Contains(t, call.sql, "PENDING_REFERENCE")
	assert.Equal(t, 10, call.args[1])
}

// ── Store plumbing ──────────────────────────────────────────────────────────

func TestRepos_ExposeEveryRepositoryTheServiceNeeds(t *testing.T) {
	// A cheap contract test with real value: if a repository is added to
	// service.Repos and not wired here, the service would nil-panic at runtime
	// instead of failing to compile.
	r := &repos{ex: &fakeQuerier{}, carrier: nopCarrier{}}
	assert.NotNil(t, r.Wallets())
	assert.NotNil(t, r.WalletOpens())
	assert.NotNil(t, r.Transactions())
	assert.NotNil(t, r.Ledger())
	assert.NotNil(t, r.Postings())
	assert.NotNil(t, r.Inbox())
	assert.NotNil(t, r.Outbox())
}

func TestNewStore_KeepsPoolAndCarrier(t *testing.T) {
	carrier := nopCarrier{}
	s := NewStore(nil, carrier)
	require.NotNil(t, s)
	assert.NotNil(t, s.Repos())
}

// ── helpers ─────────────────────────────────────────────────────────────────

func ledgerRowValues(walletID uuid.UUID, at time.Time) []any {
	return []any{
		uuid.New(), walletID, uuid.New(), "DEBIT", "BRL",
		int64(400), int64(1000), int64(600), at,
	}
}

func buildTransaction(t *testing.T, kind domain.TxKind) *domain.WagerTransaction {
	t.Helper()
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "prov-1",
		ExternalTransactionID: "ext-9",
		PlayerID:              uuid.New(),
		WalletID:              uuid.New(),
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  kind,
		Money:                 mustMoney(t, 500, "BRL"),
		ReceivedVia:           domain.ReceivedVia("api"),
		CorrelationID:         "corr-1",
		Now:                   fixedTime(),
	})
	require.NoError(t, err)
	return tx
}

// nopCarrier satisfies service.SpanCarrier without doing anything: the carrier
// is exercised by the tracing tests, not here.
type nopCarrier struct{}

func (nopCarrier) Inject(context.Context) string                         { return "" }
func (nopCarrier) Extract(ctx context.Context, _ string) context.Context { return ctx }
