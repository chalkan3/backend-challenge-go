package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Second block of adapter-contract tests: the repositories and code paths the
// first block left at 0%, chosen because they are reachable without a database.
// Anything that genuinely needs a live PostgreSQL — NewPool, the happy path of
// MigrateUp/Down — is left to the `integration` tag on purpose.

// ── remaining repository methods ────────────────────────────────────────────

func TestWalletRepo_GetByID_TranslatesNoRows(t *testing.T) {
	f := &fakeQuerier{row: errRow{err: pgx.ErrNoRows}}
	_, err := (&walletRepo{ex: f}).GetByID(context.Background(), uuid.New())
	assert.ErrorIs(t, err, service.ErrRepoNotFound)
}

func TestTxRepo_GetByProviderAndExternal_BindsProviderScope(t *testing.T) {
	f := &fakeQuerier{row: errRow{err: pgx.ErrNoRows}}
	_, err := (&txRepo{ex: f}).GetByProviderAndExternal(context.Background(), "prov-1", "ext-9")
	require.Error(t, err)

	// A provider's own transaction ids are only unique within that provider, so
	// the lookup must always be scoped by both columns.
	call := f.lastQueryRow(t)
	assert.Contains(t, call.sql, "provider_id = $1 AND external_transaction_id = $2")
	assert.Equal(t, "prov-1", call.args[0])
	assert.Equal(t, "ext-9", call.args[1])
}

func TestPostingRepo_Append_RefusesAnEmptyJournal(t *testing.T) {
	f := &fakeQuerier{}
	err := (&postingRepo{ex: f}).Append(context.Background(), domain.JournalEntry{})
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnbalancedJournal)
	assert.Empty(t, f.execCalls, "an empty journal must not reach the database")
}

func TestPostingRepo_Append_WritesEveryLegInOneStatement(t *testing.T) {
	// One INSERT with unnest is the point: a journal entry is a unit and should
	// hit the database as one statement, not N.
	entry := buildJournalEntry(t)

	f := &fakeQuerier{}
	require.NoError(t, (&postingRepo{ex: f}).Append(context.Background(), entry))

	require.Len(t, f.execCalls, 1, "all legs travel in a single statement")
	call := f.execCalls[0]
	assert.Contains(t, call.sql, "INSERT INTO postings")
	assert.Contains(t, call.sql, "unnest($1::uuid[])")
	require.Len(t, call.args, 8)
	assert.Equal(t, entry.TransactionID(), call.args[1])
	assert.Equal(t, "BRL", call.args[5])

	// The parallel arrays must agree in length, or unnest silently produces a
	// short journal.
	ids := call.args[0].([]uuid.UUID)
	kinds := call.args[2].([]string)
	refs := call.args[3].([]string)
	dirs := call.args[4].([]string)
	amounts := call.args[6].([]int64)
	for name, n := range map[string]int{
		"ids": len(ids), "kinds": len(kinds), "refs": len(refs), "dirs": len(dirs), "amounts": len(amounts),
	} {
		assert.Equal(t, len(entry.Postings()), n, "parallel array %q has the wrong length", name)
	}
}

func TestOutboxRepo_Enqueue_QueuesOneRowPerEnvelope(t *testing.T) {
	envelopes := []domain.Envelope{
		buildEnvelope(uuid.New()),
		buildEnvelope(uuid.New()),
	}

	f := &fakeQuerier{}
	require.NoError(t, (&outboxRepo{ex: f}).Enqueue(context.Background(), envelopes))
	assert.Equal(t, 1, f.sendBatchCall, "both events share one batch round trip")
}

func TestOutboxRepo_Enqueue_RejectsAnUnparseableAggregateID(t *testing.T) {
	// The aggregate id is stored as uuid, so a malformed one must fail before
	// the batch is sent rather than as an opaque SQL error.
	env := buildEnvelope(uuid.New())
	env.AggregateID = "not-a-uuid"

	f := &fakeQuerier{}
	err := (&outboxRepo{ex: f}).Enqueue(context.Background(), []domain.Envelope{env})
	require.Error(t, err)
	assert.Zero(t, f.sendBatchCall, "a bad payload must not reach the database")
}

func TestOutboxRepo_Enqueue_UsesTheInjectedTraceParent(t *testing.T) {
	// The trace context is captured once before the loop: every event committed
	// by the same transaction belongs to the same request. Reading it per event
	// would also be correct but would repeat the Inject call for no gain.
	carrier := &recordingCarrier{value: "00-trace-span-01"}
	env := buildEnvelope(uuid.New())

	f := &fakeQuerier{}
	require.NoError(t, (&outboxRepo{ex: f, carrier: carrier}).
		Enqueue(context.Background(), []domain.Envelope{env}))
	assert.Equal(t, 1, carrier.calls, "Inject is called once, not once per event")
	assert.Equal(t, 1, f.sendBatchCall)
}

func TestOutboxRepo_Enqueue_NilCarrierIsTolerated(t *testing.T) {
	// Wiring without tracing (or a test) must not panic: the column accepts "".
	f := &fakeQuerier{}
	require.NoError(t, (&outboxRepo{ex: f}).Enqueue(context.Background(), []domain.Envelope{buildEnvelope(uuid.New())}))
	assert.Equal(t, 1, f.sendBatchCall)
}

func TestOutboxRepo_ClaimBatch_PropagatesQueryError(t *testing.T) {
	boom := errors.New("deadlock detected")
	f := &fakeQuerier{queryErr: boom}
	_, err := (&outboxRepo{ex: f}).ClaimBatch(context.Background(), fixedTime(), time.Minute, 5)
	assert.ErrorIs(t, err, boom)
}

func TestOutboxRepo_ClaimBatch_BuildsItemsFromRows(t *testing.T) {
	eventID, aggID := uuid.New(), uuid.New()
	payload := `{"eventId":"e1","eventType":"wallet.balance.changed","eventVersion":1}`
	// Column order mirrors the RETURNING clause: event_id (uuid), aggregate_id
	// (uuid), event_type, attempts, payload::text, trace_parent.
	rows := newStubRows([]any{
		eventID, aggID, "wallet.balance.changed", 1, payload, "00-trace-span-01",
	})
	f := &fakeQuerier{queryRows: rows}

	items, err := (&outboxRepo{ex: f}).ClaimBatch(context.Background(), fixedTime(), time.Minute, 5)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, eventID, items[0].EventID)
	assert.Equal(t, aggID.String(), items[0].AggregateID)
	assert.Equal(t, "wallet.balance.changed", items[0].EventType)
	assert.Equal(t, 1, items[0].Attempts, "claiming bumps the attempt counter in SQL")
	assert.Equal(t, "00-trace-span-01", items[0].TraceParent,
		"the trace context must survive the round trip so the relay resumes the original trace")
	assert.JSONEq(t, payload, string(items[0].Payload))
}

func TestOutboxRepo_ClaimBatch_PropagatesRowIterationError(t *testing.T) {
	boom := errors.New("connection lost mid-stream")
	rows := newStubRows()
	rows.err = boom
	f := &fakeQuerier{queryRows: rows}

	_, err := (&outboxRepo{ex: f}).ClaimBatch(context.Background(), fixedTime(), time.Minute, 5)
	assert.ErrorIs(t, err, boom)
}

func TestLedgerRepo_ListByWallet_PropagatesRowIterationError(t *testing.T) {
	boom := errors.New("connection lost")
	rows := newStubRows(ledgerRowValues(uuid.New(), fixedTime()))
	rows.err = boom
	f := &fakeQuerier{queryRows: rows}

	_, _, err := (&ledgerRepo{ex: f}).ListByWallet(context.Background(), uuid.New(), nil, 5)
	assert.ErrorIs(t, err, boom)
}

func TestLedgerRepo_Append_PropagatesExecError(t *testing.T) {
	boom := errors.New("deadlock detected")
	w := mustWallet(t, "BRL")
	entry, err := domain.NewLedgerEntry(w.ID(), uuid.New(), domain.Movement{
		HasChange: true, Direction: domain.DirectionDebit,
		Amount:        mustMoney(t, 100, "BRL"),
		BalanceBefore: mustMoney(t, 100, "BRL"),
		BalanceAfter:  mustMoney(t, 0, "BRL"),
		WalletVersion: 2,
	}, fixedTime())
	require.NoError(t, err)

	f := &fakeQuerier{execErr: boom}
	assert.ErrorIs(t, (&ledgerRepo{ex: f}).Append(context.Background(), entry), boom)
}

// ── migrations: the paths that fail before touching a server ────────────────

func TestMigrate_InvalidDSNFailsFast(t *testing.T) {
	// openGoose parses the DSN before dialling, so a malformed one is rejected
	// without a server. This is the cheap half of the migration contract; the
	// happy path is applied against a real database by the integration suite.
	ctx := context.Background()
	tests := []struct {
		name string
		dsn  string
		run  func(context.Context, string) error
	}{
		{"up", "not a valid dsn ://", MigrateUp},
		{"down", "://still-not-valid", MigrateDown},
		{"status", "\x00invalid", MigrateStatus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.run(ctx, tc.dsn))
		})
	}
}

func TestOpenGoose_RejectsAMalformedDSN(t *testing.T) {
	p, db, err := openGoose("postgres://user:pw@host:notaport/db")
	require.Error(t, err)
	assert.Nil(t, p)
	assert.Nil(t, db)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func buildJournalEntry(t *testing.T) domain.JournalEntry {
	t.Helper()
	walletID, txID := uuid.New(), uuid.New()

	player, err := domain.NewAccount(domain.AccountPlayerWallet, walletID.String())
	require.NoError(t, err)
	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)

	debit, err := domain.NewPosting(player, domain.DirectionDebit, mustMoney(t, 500, "BRL"))
	require.NoError(t, err)
	credit, err := domain.NewPosting(house, domain.DirectionCredit, mustMoney(t, 500, "BRL"))
	require.NoError(t, err)

	entry, err := domain.NewJournalEntry(txID, "BRL", []domain.Posting{debit, credit}, fixedTime())
	require.NoError(t, err)
	return entry
}

func buildEnvelope(walletID uuid.UUID) domain.Envelope {
	return domain.Envelope{
		EventID:       "e1",
		EventType:     "wallet.balance.changed",
		AggregateType: "wallet",
		AggregateID:   walletID.String(),
		CorrelationID: "corr-1",
		OccurredAt:    fixedTime(),
		Version:       1,
		Data:          map[string]any{"walletId": walletID.String()},
	}
}

type recordingCarrier struct {
	value string
	calls int
}

func (c *recordingCarrier) Inject(context.Context) string {
	c.calls++
	return c.value
}

func (c *recordingCarrier) Extract(ctx context.Context, _ string) context.Context { return ctx }
