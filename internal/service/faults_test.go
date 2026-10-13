// This file pins the DEFENSIVE error paths of the use cases: every branch
// that exists only to propagate an infrastructure failure untouched. They are
// invisible to the happy-path fakes (which never fail) and to the end-to-end
// suite (where Postgres, SQS and the clock usually behave), yet they are the
// code that decides whether a partial failure aborts cleanly or corrupts
// state.
//
// The technique is systematic fault injection. A wrapper store embeds a fully
// working store (ledgerStore, which answers the ledger reads too) and routes
// every repository port through an adapter that delegates to the real one
// unless a fault is armed for that call site. A table then walks the surface:
// for each injectable call site the use case must return that error unchanged
// (identity, not just "some error") and must not have persisted a
// half-applied state.
package service_test

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// errInjected is the single sentinel every fault returns. Asserting on it by
// identity is what distinguishes "the use case propagated the failure" from
// "the use case returned an unrelated error of its own".
var errInjected = errors.New("injected infrastructure fault")

// faults is the mutable fault set shared by the wrapper adapters. A nil value
// disables a fault; a non-nil one makes every call at that site fail with it.
type faults struct {
	walletCreate   error
	walletUpdate   error
	walletGetByID  error
	walletGetLock  error
	walletByPlayer error

	openRecord error
	openLookup error

	txInsert       error
	txSave         error
	txGetByID      error
	txGetByKey     error
	txByExternal   error
	txClaimPending error
	txProcessedRev error

	ledgerAppend    error
	ledgerList      error
	ledgerReconcile error

	postingAppend error

	inboxBegin    error
	inboxComplete error

	outboxEnqueue error
	outboxStats   error

	withinTx error

	// keyLookups counts GetByProviderAndKey calls shared across the (fresh)
	// repos handed to each transaction, so a fault can be aimed at the Nth
	// lookup rather than the first.
	keyLookups *int
	// txGetByKeyAfter arms txGetByKey only from the Nth GetByProviderAndKey
	// call onwards (0 = from the first).
	txGetByKeyAfter int
}

// faultStore wraps a working ledgerStore and routes every repository through a
// fault-aware wrapper. Reusing the real store underneath (instead of stubbing
// return values) keeps the happy-path state machine intact, so a test can
// assert that a failure left the store untouched — and so a fault at call
// N does not prevent earlier calls from having really happened.
type faultStore struct {
	*ledgerStore
	f faults
}

func newFaultStore(t *testing.T, opening string) *faultStore {
	t.Helper()
	return &faultStore{ledgerStore: newLedgerStore(t, opening)}
}

func (s *faultStore) Repos() service.Repos { return s.repos() }

// repos mirrors ledgerRepos but with the fault-aware ports. The delegate used
// by every adaptor is the underlying ledgerStore's own repos, so the ledger
// reads keep working.
func (s *faultStore) repos() service.Repos {
	return &faultRepos{
		delegate: s.ledgerStore.Repos(),
		store:    s.ledgerStore.fakeStore,
		f:        &s.f,
	}
}

// WithinTx runs the callback under the fault-aware repos, and models the
// branch that models "could not even start the transaction".
func (s *faultStore) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	if s.f.withinTx != nil {
		return s.f.withinTx
	}
	return fn(ctx, s.repos())
}

func (s *faultStore) wagerService() *service.WagerService {
	return service.NewWagerService(s, slog.Default(), s.instrument())
}

func (s *faultStore) walletService() *service.WalletService {
	return service.NewWalletService(s, slog.Default(), s.instrument())
}

type faultRepos struct {
	delegate service.Repos
	store    *fakeStore
	f        *faults
}

func (r *faultRepos) Wallets() service.WalletRepository {
	return &faultWallets{r.delegate.Wallets(), r.f}
}
func (r *faultRepos) WalletOpens() service.WalletOpenRepository {
	return &faultOpens{r.delegate.WalletOpens(), r.f}
}
func (r *faultRepos) Transactions() service.TransactionRepository {
	return &faultTxs{r.delegate.Transactions(), r.f}
}
func (r *faultRepos) Ledger() service.LedgerRepository { return &faultLedger{r.delegate.Ledger(), r.f} }
func (r *faultRepos) Postings() service.PostingRepository {
	return &faultPostings{r.delegate.Postings(), r.f}
}
func (r *faultRepos) Inbox() service.InboxRepository   { return &faultInbox{r.delegate.Inbox(), r.f} }
func (r *faultRepos) Outbox() service.OutboxRepository { return &faultOutbox{r.delegate.Outbox(), r.f} }

// --- per-port fault adapters (each fails only when armed) -------------------

type faultWallets struct {
	next service.WalletRepository
	f    *faults
}

func (r *faultWallets) Create(ctx context.Context, w *domain.Wallet) error {
	if r.f.walletCreate != nil {
		return r.f.walletCreate
	}
	return r.next.Create(ctx, w)
}

func (r *faultWallets) UpdateBalance(ctx context.Context, w *domain.Wallet) error {
	if r.f.walletUpdate != nil {
		return r.f.walletUpdate
	}
	return r.next.UpdateBalance(ctx, w)
}

func (r *faultWallets) GetByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	if r.f.walletGetByID != nil {
		return nil, r.f.walletGetByID
	}
	return r.next.GetByID(ctx, id)
}

func (r *faultWallets) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	if r.f.walletGetLock != nil {
		return nil, r.f.walletGetLock
	}
	return r.next.GetForUpdate(ctx, id)
}

func (r *faultWallets) GetByPlayerAndCurrency(ctx context.Context, p uuid.UUID, c string, l bool) (*domain.Wallet, error) {
	if r.f.walletByPlayer != nil {
		return nil, r.f.walletByPlayer
	}
	return r.next.GetByPlayerAndCurrency(ctx, p, c, l)
}

// ListIDs is not faulted: no reconciliation-sweep test injects a listing
// failure through this fake, so it delegates straight through.
func (r *faultWallets) ListIDs(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	return r.next.ListIDs(ctx, after, limit)
}

type faultOpens struct {
	next service.WalletOpenRepository
	f    *faults
}

func (r *faultOpens) Record(ctx context.Context, key, hash string, id uuid.UUID) error {
	if r.f.openRecord != nil {
		return r.f.openRecord
	}
	return r.next.Record(ctx, key, hash, id)
}

func (r *faultOpens) Lookup(ctx context.Context, key string) (uuid.UUID, string, error) {
	if r.f.openLookup != nil {
		return uuid.Nil, "", r.f.openLookup
	}
	return r.next.Lookup(ctx, key)
}

type faultTxs struct {
	next service.TransactionRepository
	f    *faults
}

func (r *faultTxs) Insert(ctx context.Context, t *domain.WagerTransaction) error {
	if r.f.txInsert != nil {
		return r.f.txInsert
	}
	return r.next.Insert(ctx, t)
}

func (r *faultTxs) Save(ctx context.Context, t *domain.WagerTransaction) error {
	if r.f.txSave != nil {
		return r.f.txSave
	}
	return r.next.Save(ctx, t)
}

func (r *faultTxs) GetByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	if r.f.txGetByID != nil {
		return nil, r.f.txGetByID
	}
	return r.next.GetByID(ctx, id)
}

func (r *faultTxs) GetByProviderAndKey(ctx context.Context, p, k string) (*domain.WagerTransaction, error) {
	if r.f.keyLookups != nil {
		*r.f.keyLookups++
		if r.f.txGetByKeyAfter != 0 {
			// Threshold mode: only the Nth-and-later lookups fail.
			if *r.f.keyLookups >= r.f.txGetByKeyAfter {
				return nil, r.f.txGetByKey
			}
			return r.next.GetByProviderAndKey(ctx, p, k)
		}
	}
	if r.f.txGetByKey != nil {
		return nil, r.f.txGetByKey
	}
	return r.next.GetByProviderAndKey(ctx, p, k)
}

func (r *faultTxs) GetByProviderAndExternal(ctx context.Context, p, e string) (*domain.WagerTransaction, error) {
	if r.f.txByExternal != nil {
		return nil, r.f.txByExternal
	}
	return r.next.GetByProviderAndExternal(ctx, p, e)
}

func (r *faultTxs) ClaimPendingReferences(ctx context.Context, now time.Time, limit int) ([]*domain.WagerTransaction, error) {
	if r.f.txClaimPending != nil {
		return nil, r.f.txClaimPending
	}
	return r.next.ClaimPendingReferences(ctx, now, limit)
}

func (r *faultTxs) ExistsProcessedReversal(ctx context.Context, ref uuid.UUID, k domain.TxKind) (bool, error) {
	if r.f.txProcessedRev != nil {
		return false, r.f.txProcessedRev
	}
	return r.next.ExistsProcessedReversal(ctx, ref, k)
}

type faultLedger struct {
	next service.LedgerRepository
	f    *faults
}

func (r *faultLedger) Append(ctx context.Context, e domain.LedgerEntry) error {
	if r.f.ledgerAppend != nil {
		return r.f.ledgerAppend
	}
	return r.next.Append(ctx, e)
}

func (r *faultLedger) ListByWallet(ctx context.Context, id uuid.UUID, c *service.LedgerCursor, n int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	if r.f.ledgerList != nil {
		return nil, nil, r.f.ledgerList
	}
	return r.next.ListByWallet(ctx, id, c, n)
}

func (r *faultLedger) Reconcile(ctx context.Context, id uuid.UUID) (int64, int64, int64, error) {
	if r.f.ledgerReconcile != nil {
		return 0, 0, 0, r.f.ledgerReconcile
	}
	return r.next.Reconcile(ctx, id)
}

// faultPostings fails Append only when armed, so a test can prove the journal
// write is inside the transaction boundary: a client that fails it must leave
// the ledger, the balance and the transaction row untouched too.
type faultPostings struct {
	next service.PostingRepository
	f    *faults
}

func (r *faultPostings) Append(ctx context.Context, e domain.JournalEntry) error {
	if r.f.postingAppend != nil {
		return r.f.postingAppend
	}
	return r.next.Append(ctx, e)
}

type faultInbox struct {
	next service.InboxRepository
	f    *faults
}

func (r *faultInbox) Begin(ctx context.Context, c, m, h string) (service.InboxOutcome, error) {
	if r.f.inboxBegin != nil {
		return service.InboxOutcome{}, r.f.inboxBegin
	}
	return r.next.Begin(ctx, c, m, h)
}

func (r *faultInbox) Complete(ctx context.Context, c, m string, at time.Time) error {
	if r.f.inboxComplete != nil {
		return r.f.inboxComplete
	}
	return r.next.Complete(ctx, c, m, at)
}

type faultOutbox struct {
	next service.OutboxRepository
	f    *faults
}

func (r *faultOutbox) Enqueue(ctx context.Context, e []domain.Envelope) error {
	if r.f.outboxEnqueue != nil {
		return r.f.outboxEnqueue
	}
	return r.next.Enqueue(ctx, e)
}

func (r *faultOutbox) ClaimBatch(ctx context.Context, n time.Time, l time.Duration, i int) ([]service.OutboxItem, error) {
	return r.next.ClaimBatch(ctx, n, l, i)
}

func (r *faultOutbox) MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error {
	return r.next.MarkPublished(ctx, id, at)
}

func (r *faultOutbox) MarkFailed(ctx context.Context, id uuid.UUID, at time.Time, e string) error {
	return r.next.MarkFailed(ctx, id, at, e)
}

func (r *faultOutbox) Stats(ctx context.Context) (int64, float64, error) {
	if r.f.outboxStats != nil {
		return 0, 0, r.f.outboxStats
	}
	return r.next.Stats(ctx)
}

// --- submission fault matrix ------------------------------------------------

// TestSubmitPropagatesInfrastructureFaults drives the whole Submit pipeline
// with a fault at each call site in turn. For every site the injected error
// must surface by identity. Additionally, a fault that strikes BEFORE the
// transaction row is written must not leave a row behind (the fake store has
// no rollback, so the check is scoped to those sites); the sites after the
// INSERT are covered by the real PostgreSQL suite, where WithinTx rolls the
// whole transaction back.
func TestSubmitPropagatesInfrastructureFaults(t *testing.T) {
	cases := []struct {
		name         string
		apply        func(*faults)
		beforeInsert bool
	}{
		{"within tx", func(f *faults) { f.withinTx = errInjected }, true},
		{"idempotency lookup", func(f *faults) { f.txGetByKey = errInjected }, true},
		{"external id lookup", func(f *faults) { f.txByExternal = errInjected }, true},
		{"wallet lock", func(f *faults) { f.walletGetLock = errInjected }, true},
		{"transaction insert", func(f *faults) { f.txInsert = errInjected }, true},
		{"wallet update", func(f *faults) { f.walletUpdate = errInjected }, false},
		{"ledger append", func(f *faults) { f.ledgerAppend = errInjected }, false},
		{"outbox enqueue", func(f *faults) { f.outboxEnqueue = errInjected }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultStore(t, "100.00")
			in := betInput(store.wallet(t))
			tc.apply(&store.f)

			_, err := store.wagerService().Submit(context.Background(), in)

			require.Error(t, err)
			assert.ErrorIs(t, err, errInjected,
				"the use case must propagate the infrastructure error unchanged")
			if tc.beforeInsert {
				assert.Empty(t, store.txByID,
					"a fault before the INSERT must leave no transaction row")
			}
		})
	}
}

// TestSubmitWalletNotFoundVsRepoFault draws the line the code draws: a
// missing wallet is a business answer (404, not an error), while any other
// repository failure must stay an infrastructure error. Conflating the two
// would report a database outage as "wallet not found".
func TestSubmitWalletNotFoundVsRepoFault(t *testing.T) {
	t.Run("missing wallet is not found, not a fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		in := betInput(store.wallet(t))
		in.WalletID = uuid.NewString()

		_, err := store.wagerService().Submit(context.Background(), in)
		require.Error(t, err)
		assert.True(t, service.IsNotFound(err))
		assert.NotErrorIs(t, err, errInjected)
	})

	t.Run("any other lock failure is an infrastructure fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		in := betInput(store.wallet(t))
		store.f.walletGetLock = errInjected

		_, err := store.wagerService().Submit(context.Background(), in)
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
		assert.False(t, service.IsNotFound(err),
			"a repository fault must not be disguised as a 404")
	})
}

// TestReplayReadPathFaults covers replayResult, reached only when the
// idempotency lookup finds a row that was never processed (e.g. a pending
// reference). The wallet read that reconstructs the reported balance has two
// failure branches: a disappeared wallet degrades to a zero balance (tested
// elsewhere), while a real fault must surface unchanged.
func TestReplayReadPathFaults(t *testing.T) {
	store := newFaultStore(t, "100.00")
	in := betInput(store.wallet(t))
	svc := store.wagerService()

	// A REFUND against an unknown reference parks the transaction in
	// PENDING_REFERENCE: a persisted row with no balance movement, exactly
	// what sends the next Submit down the replay branch.
	in.Kind = "REFUND"
	in.ReferenceExternalTransactionID = "missing-ref"
	_, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	// The replay now reads the wallet back to report the live balance — and
	// that read is made to fail.
	store.f.walletGetByID = errInjected
	_, err = svc.Submit(context.Background(), in)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInjected)
}

// TestOpenFaults pins the wallet-opening use case, whose defensive branches
// guard the "record the idempotency identity, then create" sequence. The
// contract is only that the injected failure surfaces: the store underneath
// has no rollback, so a mid-sequence abort is Simulation-only, but the error
// must never be swallowed into an empty success.
func TestOpenFaults(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*faults)
	}{
		{"wallet create", func(f *faults) { f.walletCreate = errInjected }},
		{"idempotency record", func(f *faults) { f.openRecord = errInjected }},
		{"idempotency lookup", func(f *faults) { f.openLookup = errInjected }},
		{"ledger append", func(f *faults) { f.ledgerAppend = errInjected }},
		{"outbox enqueue", func(f *faults) { f.outboxEnqueue = errInjected }},
		{"within tx", func(f *faults) { f.withinTx = errInjected }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultStore(t, "100.00")
			tc.apply(&store.f)

			_, err := store.walletService().Open(context.Background(), service.OpenWalletInput{
				PlayerID:       uuid.NewString(),
				InitialAmount:  "5.00",
				Currency:       "BRL",
				IdempotencyKey: "open-" + uuid.NewString(),
			})

			require.Error(t, err)
			assert.ErrorIs(t, err, errInjected)
		})
	}
}

// TestReadPathFaults covers the read-only use cases: Get, ListLedger and
// Reconcile. Their failures are less dangerous than a half-written Submit, but
// swallowing one would turn a database problem into a misleading empty page or
// a falsely "consistent" reconciliation.
func TestReadPathFaults(t *testing.T) {
	walletIDOf := func(t *testing.T, s *faultStore) string {
		t.Helper()
		return s.wallet(t).ID().String()
	}

	t.Run("get: repository fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		store.f.walletGetByID = errInjected

		_, err := store.walletService().Get(context.Background(), walletIDOf(t, store))
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
	})

	t.Run("list ledger: repository fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		store.f.ledgerList = errInjected

		_, _, err := store.walletService().ListLedger(context.Background(), walletIDOf(t, store), "", 10)
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
	})

	t.Run("reconcile: wallet fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		store.f.walletGetByID = errInjected

		_, err := store.walletService().Reconcile(context.Background(), walletIDOf(t, store))
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
	})

	t.Run("reconcile: ledger fault", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		store.f.ledgerReconcile = errInjected

		_, err := store.walletService().Reconcile(context.Background(), walletIDOf(t, store))
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
	})
}

// --- input-shape validation ------------------------------------------------

// TestSubmitRejectsMalformedInput walks buildTransaction's validation ladder:
// every field the wire format requires, in the order the code checks them.
// These branches are the service's first line of defense — they run before any
// transaction is opened, so each one must produce a ValidationError and reach
// no repository at all.
func TestSubmitRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*service.ExternalInput)
	}{
		{"missing idempotency key", func(in *service.ExternalInput) { in.IdempotencyKey = "" }},
		{"malformed player id", func(in *service.ExternalInput) { in.PlayerID = "not-a-uuid" }},
		{"malformed wallet id", func(in *service.ExternalInput) { in.WalletID = "not-a-uuid" }},
		{"malformed amount", func(in *service.ExternalInput) { in.Amount = "abc" }},
		{"empty currency", func(in *service.ExternalInput) { in.Amount = "10.00"; in.Currency = "" }},
		{"unknown kind", func(in *service.ExternalInput) { in.Kind = "NOT_A_KIND" }},
		{"internal-only opening kind", func(in *service.ExternalInput) { in.Kind = "OPENING" }},
		{"refund without reference", func(in *service.ExternalInput) { in.Kind = "REFUND" }},
		{"rollback without reference", func(in *service.ExternalInput) { in.Kind = "ROLLBACK" }},
		{"missing provider id", func(in *service.ExternalInput) { in.ProviderID = "" }},
		{"missing external transaction id", func(in *service.ExternalInput) { in.ExternalTransactionID = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultStore(t, "100.00")
			in := betInput(store.wallet(t))
			tc.mutate(&in)

			_, err := store.wagerService().Submit(context.Background(), in)

			require.Error(t, err)
			assert.True(t, service.IsValidation(err),
				"malformed input must be a validation failure, got %v", err)
			assert.Empty(t, store.txByID, "validation must reject before any write")
		})
	}
}

// TestOpenRejectsMalformedInput mirrors the check for the opening use case:
// the validation ladder runs before WithinTx, so a bad body never opens a
// transaction and never creates a wallet.
func TestOpenRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name  string
		input service.OpenWalletInput
	}{
		{"malformed player id", service.OpenWalletInput{PlayerID: "nope", InitialAmount: "5.00", Currency: "BRL"}},
		{"missing currency", service.OpenWalletInput{PlayerID: uuid.NewString(), InitialAmount: "5.00"}},
		{"malformed amount", service.OpenWalletInput{PlayerID: uuid.NewString(), InitialAmount: "x", Currency: "BRL"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultStore(t, "100.00")

			_, err := store.walletService().Open(context.Background(), tc.input)

			require.Error(t, err)
			assert.True(t, service.IsValidation(err))
		})
	}
}

// --- reference resolution faults -------------------------------------------

// TestResolvePendingFaults drives the reference worker's advance path with a
// fault at each call site. The worker runs against a claimed row outside
// Submit's transaction, so an unreported failure here would silently stall a
// reversal forever or, worse, half-apply it.
func TestResolvePendingFaults(t *testing.T) {
	setup := func(t *testing.T) (*faultStore, service.WagerResult) {
		t.Helper()
		store := newFaultStore(t, "100.00")
		w := store.wallet(t)
		svc := store.wagerService()

		// A REFUND without its reference parks the row as PENDING_REFERENCE.
		pend, err := svc.Submit(context.Background(), refundInput(w, "late-ext", "10.00"))
		require.NoError(t, err)
		require.Equal(t, domain.StatusPendingReference, pend.Transaction.Status())

		// The referenced BET lands afterwards, so resolution can proceed past
		// the lookup and reach the later write sites.
		late := betInput(w)
		late.ExternalTransactionID = "late-ext"
		_, err = svc.Submit(context.Background(), late)
		require.NoError(t, err)
		return store, pend
	}

	cases := []struct {
		name  string
		apply func(*faults)
	}{
		{"wallet lock", func(f *faults) { f.walletGetLock = errInjected }},
		{"reference lookup", func(f *faults) { f.txByExternal = errInjected }},
		{"reversal existence check", func(f *faults) { f.txProcessedRev = errInjected }},
		{"transaction save", func(f *faults) { f.txSave = errInjected }},
		{"wallet update", func(f *faults) { f.walletUpdate = errInjected }},
		{"ledger append", func(f *faults) { f.ledgerAppend = errInjected }},
		{"outbox enqueue", func(f *faults) { f.outboxEnqueue = errInjected }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, pend := setup(t)
			svc := store.wagerService()
			tc.apply(&store.f)

			var err error
			require.NotPanics(t, func() {
				err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
					_, e := svc.ResolvePending(ctx, r, pend.Transaction)
					return e
				})
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, errInjected)
		})
	}
}

// --- duplicate-race recovery faults ----------------------------------------

// racingStore forces the first Insert to lose the unique-index race
// (ErrDuplicateInsert), sending Submit down the duplicateRaceReplay branch.
// The recovery transaction (the SECOND WithinTx of one Submit) can then be
// faulted.
type racingStore struct {
	*faultStore
	calls      int
	failOnCall int // 0 = never
}

func (s *racingStore) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	s.calls++
	if s.failOnCall != 0 && s.calls == s.failOnCall {
		return errInjected
	}
	return fn(ctx, s.repos())
}

func (s *racingStore) Repos() service.Repos {
	r := s.faultStore.repos().(*faultRepos)
	return &faultRepos{
		delegate: r.delegate,
		store:    r.store,
		f:        r.f,
	}
}

// TestDuplicateRaceRecoveryFaults pins the two failure branches of the
// recovery path: if the fresh transaction cannot even start, or the replay
// re-read fails, the error must surface — never a silent zero result.
func TestDuplicateRaceRecoveryFaults(t *testing.T) {
	t.Run("recovery transaction fails", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		// Make the very first Insert lose, then fail the SECOND WithinTx
		// (the recovery transaction).
		store.f.txInsert = service.ErrDuplicateInsert
		rs := &racingStore{faultStore: store, failOnCall: 2}

		in := betInput(store.wallet(t))
		svc := service.NewWagerService(rs, slog.Default(), store.instrument())
		_, err := svc.Submit(context.Background(), in)
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
		assert.Equal(t, 2, rs.calls, "Submit must have opened a second transaction for the recovery")
	})

	t.Run("replay lookup fails", func(t *testing.T) {
		store := newFaultStore(t, "100.00")
		// First Insert loses; the second WithinTx runs, but the replay
		// re-read inside it faults.
		store.f.txInsert = service.ErrDuplicateInsert
		rs := &racingStore{faultStore: store}

		// The first GetByProviderAndKey (idempotency check) must return
		// not-found; only the recovery's re-read faults.
		count := 0
		store.f.keyLookups = &count
		store.f.txGetByKey = errInjected
		store.f.txGetByKeyAfter = 2

		in := betInput(store.wallet(t))
		svc := service.NewWagerService(rs, slog.Default(), store.instrument())
		_, err := svc.Submit(context.Background(), in)
		require.Error(t, err)
		assert.ErrorIs(t, err, errInjected)
		assert.GreaterOrEqual(t, count, 2, "the recovery re-read must have run")
	})
}

// TestListLedgerValidationAndCursor covers ListLedger's pure-input branches:
// a malformed wallet id, and the opaque cursor decoder on both its failure
// modes (not base64, base64-but-not-JSON). A bad cursor is a client error, not
// a 500, so it must be a ValidationError.
func TestListLedgerValidationAndCursor(t *testing.T) {
	store := newFaultStore(t, "100.00")
	svc := store.walletService()
	wid := store.wallet(t).ID().String()

	t.Run("malformed wallet id", func(t *testing.T) {
		_, _, err := svc.ListLedger(context.Background(), "not-a-uuid", "", 10)
		require.Error(t, err)
		assert.True(t, service.IsValidation(err))
	})

	t.Run("cursor not base64", func(t *testing.T) {
		_, _, err := svc.ListLedger(context.Background(), wid, "!!!not base64!!!", 10)
		require.Error(t, err)
		assert.True(t, service.IsValidation(err), "a bad cursor is a client error, not a 500")
	})

	t.Run("cursor base64 but not JSON", func(t *testing.T) {
		// Valid base64 that does not decode to a JSON cursor.
		bad := base64RawURLEncode([]byte("not json at all"))
		_, _, err := svc.ListLedger(context.Background(), wid, bad, 10)
		require.Error(t, err)
		assert.True(t, service.IsValidation(err))
	})
}

// TestReconcileValidation covers Reconcile's parse guard, mirroring Get and
// ListLedger: a non-UUID id is rejected before any query runs.
func TestReconcileValidation(t *testing.T) {
	store := newFaultStore(t, "100.00")
	_, err := store.walletService().Reconcile(context.Background(), "not-a-uuid")
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

// TestGetValidation covers Get's parse guard.
func TestGetValidation(t *testing.T) {
	store := newFaultStore(t, "100.00")
	_, err := store.walletService().Get(context.Background(), "not-a-uuid")
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

// TestOpenUniquenessGuardFault covers createWallet's first repository call:
// the per-(player, currency) uniqueness lookup. A fault there must abort the
// opening rather than fall through to a duplicate wallet.
func TestOpenUniquenessGuardFault(t *testing.T) {
	store := newFaultStore(t, "100.00")
	// Arm only the uniqueness lookup; the create path must stop there.
	store.f.walletByPlayer = errInjected

	_, err := store.walletService().Open(context.Background(), service.OpenWalletInput{
		PlayerID:       uuid.NewString(),
		InitialAmount:  "5.00",
		Currency:       "BRL",
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, errInjected)
}

func base64RawURLEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// TestReversalOfRejectedReferenceIsRejected pins the rule that a REFUND may
// only reference a PROCESSED BET. A reference that finished WITHOUT success
// (here: a BET rejected for insufficient funds) is terminal, and reversing it
// would fabricate money: the original debit never happened.
func TestReversalOfRejectedReferenceIsRejected(t *testing.T) {
	store := newFaultStore(t, "5.00") // small balance so the BET cannot be paid
	w := store.wallet(t)
	svc := store.wagerService()

	// A BET larger than the balance is persisted as REJECTED (INSUFFICIENT_FUNDS).
	rejected := betInput(w)
	rejected.ExternalTransactionID = "rejected-bet-" + uuid.NewString()
	rejected.Amount = "100.00"
	_, err := svc.Submit(context.Background(), rejected)
	require.NoError(t, err)

	// The REFUND pointing at it must be rejected with REFERENCE_NOT_PROCESSED.
	refund := refundInput(w, rejected.ExternalTransactionID, "100.00")
	res, err := svc.Submit(context.Background(), refund)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailReferenceNotProcessed, res.Transaction.FailureCode())
	assert.Equal(t, "5.00", store.wallet(t).Balance().String(), "no money may move")
}

// TestOpenZeroInitialBalanceWritesNoFinancialRows pins the branch where the
// opening amount is zero: the wallet is created, but no OPENING transaction,
// ledger entry or balance event is produced. Fabricating a zero-value ledger
// row would break the invariant "every ledger line moves exactly what it says".
func TestOpenZeroInitialBalanceWritesNoFinancialRows(t *testing.T) {
	store := newFaultStore(t, "100.00")
	svc := store.walletService()

	beforeTxs := len(store.txByID)
	beforeLedger := len(store.ledger)

	view, err := svc.Open(context.Background(), service.OpenWalletInput{
		PlayerID:       uuid.NewString(),
		InitialAmount:  "0.00",
		Currency:       "BRL",
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.NoError(t, err)
	assert.True(t, view.Balance.IsZero())
	assert.Equal(t, beforeTxs, len(store.txByID), "a zero opening writes no transaction row")
	assert.Equal(t, beforeLedger, len(store.ledger), "a zero opening writes no ledger entry")
}

// TestOpenConflictWhenCreateLosesRace covers the Create→ErrDuplicateInsert
// branch: the per-player uniqueness guard passed (the row was absent when it
// ran), yet the insert lost to a concurrent opener. The service must translate
// that into the same WALLET_ALREADY_EXISTS conflict rather than a 500.
func TestOpenConflictWhenCreateLosesRace(t *testing.T) {
	store := newFaultStore(t, "100.00")
	svc := store.walletService()
	store.f.walletCreate = service.ErrDuplicateInsert

	_, err := svc.Open(context.Background(), service.OpenWalletInput{
		PlayerID:       uuid.NewString(),
		InitialAmount:  "5.00",
		Currency:       "BRL",
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.Error(t, err)
	assert.True(t, service.IsConflict(err), "a lost create race must be the WALLET_ALREADY_EXISTS conflict")
}

// TestOpenUniquenessGuardConflict covers the branch where the uniqueness lookup
// FINDS an existing wallet: Open must refuse to create a second wallet for the
// same (player, currency).
func TestOpenUniquenessGuardConflict(t *testing.T) {
	store := newFaultStore(t, "100.00")
	svc := store.walletService()
	existing := store.wallet(t)

	_, err := svc.Open(context.Background(), service.OpenWalletInput{
		PlayerID:       existing.PlayerID().String(),
		InitialAmount:  "5.00",
		Currency:       existing.Currency(),
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.Error(t, err)
	assert.True(t, service.IsConflict(err))
}
