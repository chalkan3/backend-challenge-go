// Package service_test — this file widens the unit suite over the read and
// reference-resolution paths of the use cases that the fake store did not yet
// exercise: wallet reads and pagination, ledger reconciliation, the
// transaction lookup endpoints the HTTP transport exposes, and the
// REFUND/ROLLBACK reference machinery (schedule -> resolve -> apply), which is
// the most stateful and least directly reachable part of the service layer.
//
// The shared fakes in wager_test.go intentionally keep their behaviour: this
// file adds read-capable adapters (ledgerStore / ledgerLedger) instead of
// changing the ones the existing tests depend on.
package service_test

import (
	"context"
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

// --- a ledger-capable store -------------------------------------------------

// ledgerStore is a fakeStore that additionally answers the ledger reads
// (ListByWallet, Reconcile). It reuses the in-memory ledger slice the wager
// flow fills, so a wallet moved through the use case can be read back and
// reconciled exactly like production. openingMinor carries the wallet's
// opening credit, which production records as the first ledger entry (the
// fake store creates the wallet directly, so it is modelled explicitly).
type ledgerStore struct {
	*fakeStore
	openingMinor int64
}

func newLedgerStore(t *testing.T, opening string) *ledgerStore {
	t.Helper()
	m := mustMoney(t, opening, "BRL")
	return &ledgerStore{fakeStore: newFakeStore(t, opening), openingMinor: m.AmountMinor()}
}

func (f *ledgerStore) Repos() service.Repos {
	return &ledgerRepos{store: f.fakeStore, openingMinor: f.openingMinor}
}

// WithinTx must be overridden: the embedded fakeStore's version supplies
// *its own* Repos (the stub ledger), so the ledger capability would be lost
// inside the transaction. Delegating to this type's Repos keeps the ledger
// reads visible to the use case.
func (f *ledgerStore) WithinTx(ctx context.Context, fn func(ctx context.Context, r service.Repos) error) error {
	return fn(ctx, f.Repos())
}

func (f *ledgerStore) walletService() *service.WalletService {
	return service.NewWalletService(f, slog.Default(), f.instrument())
}

type ledgerRepos struct {
	store        *fakeStore
	openingMinor int64
}

func (r *ledgerRepos) Wallets() service.WalletRepository {
	return fakeWallets{r.store}
}
func (r *ledgerRepos) WalletOpens() service.WalletOpenRepository {
	return fakeWalletOpens{r.store}
}
func (r *ledgerRepos) Transactions() service.TransactionRepository {
	return fakeTxs{r.store}
}
func (r *ledgerRepos) Ledger() service.LedgerRepository {
	return ledgerLedger{s: r.store, openingMinor: r.openingMinor}
}
func (r *ledgerRepos) Inbox() service.InboxRepository   { return fakeInbox{} }
func (r *ledgerRepos) Outbox() service.OutboxRepository { return fakeOutbox{r.store} }

// ledgerLedger is fakeLedger with real pagination and reconciliation over the
// shared in-memory ledger slice. Ordering is the insertion order of the slice
// (tests append in chronological order), and the cursor is the last returned
// entry's (createdAt, id).
//
// openingMinor is the wallet's opening credit, which production records as the
// first ledger entry; the fake store births the wallet directly, so the amount
// is threaded through instead.
type ledgerLedger struct {
	s            *fakeStore
	openingMinor int64
}

func (r ledgerLedger) Append(ctx context.Context, e domain.LedgerEntry) error {
	return fakeLedger{r.s}.Append(ctx, e)
}
func (r ledgerLedger) ListByWallet(_ context.Context, walletID uuid.UUID, cursor *service.LedgerCursor, limit int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	var out []*domain.LedgerEntry
	for i := range r.s.ledger {
		e := r.s.ledger[i]
		if e.WalletID() != walletID {
			continue
		}
		if cursor != nil {
			// Keyset: strictly after (createdAt, id). Ties on createdAt are
			// broken by id, matching the SQL cursor.
			if e.CreatedAt().Before(cursor.CreatedAt) {
				continue
			}
			if e.CreatedAt().Equal(cursor.CreatedAt) && e.ID().String() <= cursor.ID.String() {
				continue
			}
		}
		cp := e
		out = append(out, &cp)
		if len(out) >= limit {
			break
		}
	}
	var next *service.LedgerCursor
	if len(out) == limit && len(out) > 0 {
		last := out[len(out)-1]
		next = &service.LedgerCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
	}
	return out, next, nil
}

func (r ledgerLedger) Reconcile(_ context.Context, walletID uuid.UUID) (int64, int64, int64, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.wallets[walletID]
	if !ok {
		return 0, 0, 0, service.ErrRepoNotFound
	}
	sum := r.openingMinor
	count := int64(1)
	for i := range r.s.ledger {
		e := r.s.ledger[i]
		if e.WalletID() != walletID {
			continue
		}
		count++
		if e.Direction() == domain.DirectionCredit {
			sum += e.Amount().AmountMinor()
		} else {
			sum -= e.Amount().AmountMinor()
		}
	}
	return w.Balance().AmountMinor(), sum, count, nil
}

// --- wallet reads -----------------------------------------------------------

func TestGetReturnsWalletView(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)

	view, err := store.walletService().Get(context.Background(), w.ID().String())
	require.NoError(t, err)
	assert.Equal(t, w.ID(), view.ID)
	assert.Equal(t, w.PlayerID(), view.PlayerID)
	assert.Equal(t, "BRL", view.Currency)
	assert.Equal(t, "100.00", view.Balance.String())
}

func TestGetRejectsMalformedID(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, err := store.walletService().Get(context.Background(), "not-a-uuid")
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestGetMapsMissingWalletToNotFound(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, err := store.walletService().Get(context.Background(), uuid.NewString())
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
	assert.Equal(t, service.CodeWalletNotFound, service.NotFoundCode(err))
}

// --- ledger pagination ------------------------------------------------------

func TestListLedgerPagesWithCursor(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)
	svc := store.walletService()

	// Seed three ledger entries by moving the wallet through the use case, so
	// the ledger is produced by the real code path, not hand-built fixtures.
	wager := newWagerService(store.fakeStore)
	for i := 0; i < 3; i++ {
		res, err := wager.Submit(context.Background(), betInput(w))
		require.NoError(t, err)
		require.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	}
	require.Len(t, store.ledger, 3)

	// First page of two.
	page1, next, err := svc.ListLedger(context.Background(), w.ID().String(), "", 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.NotEmpty(t, next, "a full page yields a cursor")

	// Second page of two returns the remaining entry and no cursor.
	page2, next2, err := svc.ListLedger(context.Background(), w.ID().String(), next, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Empty(t, next2, "a short page ends pagination")

	// No overlap, all entries seen exactly once.
	seen := map[string]bool{}
	for _, it := range append(page1, page2...) {
		require.False(t, seen[it.ID], "cursor must not repeat entry %s", it.ID)
		seen[it.ID] = true
		assert.Equal(t, domain.DirectionDebit, it.Direction)
	}
	assert.Len(t, seen, 3)
}

func TestListLedgerRejectsMalformedCursor(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)

	_, _, err := store.walletService().ListLedger(context.Background(), w.ID().String(), "!!!not-base64!!!", 10)
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestListLedgerRejectsMalformedWalletID(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, _, err := store.walletService().ListLedger(context.Background(), "nope", "", 10)
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestWithLedgerPageSizeClampsLimit(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)
	svc := service.NewWalletService(store, slog.Default(), store.instrument(),
		service.WithLedgerPageSize(1))

	wager := newWagerService(store.fakeStore)
	for i := 0; i < 3; i++ {
		_, err := wager.Submit(context.Background(), betInput(w))
		require.NoError(t, err)
	}
	// limit=100 must be clamped to the configured page size of 1.
	page, next, err := svc.ListLedger(context.Background(), w.ID().String(), "", 100)
	require.NoError(t, err)
	assert.Len(t, page, 1)
	assert.NotEmpty(t, next)
}

// --- reconciliation ---------------------------------------------------------

func TestReconcileReportsConsistentAfterMoves(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)
	wager := newWagerService(store.fakeStore)

	res, err := wager.Submit(context.Background(), betInput(w))
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, res.Transaction.Status())

	report, err := store.walletService().Reconcile(context.Background(), w.ID().String())
	require.NoError(t, err)
	assert.True(t, report.Consistent, "ledger must sum to the stored balance")
	assert.True(t, report.Difference.IsZero())
	assert.Equal(t, int64(2), report.CheckedEntries, "the opening entry plus the bet")
}

func TestReconcileDetectsDivergence(t *testing.T) {
	store := newLedgerStore(t, "100.00")
	w := store.wallet(t)
	wager := newWagerService(store.fakeStore)
	_, err := wager.Submit(context.Background(), betInput(w))
	require.NoError(t, err)

	// Force the ledger to sum to a value that disagrees with the stored
	// balance (simulating a historical rewrite). The report must flag the
	// divergence and instrument it.
	svc := service.NewWalletService(divergentTxm{store}, slog.Default(), store.instrument())
	report, err := svc.Reconcile(context.Background(), w.ID().String())
	require.NoError(t, err)
	assert.False(t, report.Consistent, "a ledger that does not sum to the balance is a divergence")
	assert.False(t, report.Difference.IsZero())
}

// divergentTxm wraps a ledgerStore and inflates the ledger sum by one minor
// unit on Reconcile, so the reconciliation read path can be exercised without
// reaching into the wallet aggregate.
type divergentTxm struct{ *ledgerStore }

func (d divergentTxm) Repos() service.Repos {
	return &divergentRepos{store: d.fakeStore, openingMinor: d.openingMinor}
}

func (d divergentTxm) WithinTx(ctx context.Context, fn func(ctx context.Context, r service.Repos) error) error {
	return fn(ctx, d.Repos())
}

type divergentRepos struct {
	store        *fakeStore
	openingMinor int64
}

func (r *divergentRepos) Wallets() service.WalletRepository {
	return fakeWallets{r.store}
}
func (r *divergentRepos) WalletOpens() service.WalletOpenRepository {
	return fakeWalletOpens{r.store}
}
func (r *divergentRepos) Transactions() service.TransactionRepository {
	return fakeTxs{r.store}
}
func (r *divergentRepos) Ledger() service.LedgerRepository {
	return divergentLedger{s: r.store, openingMinor: r.openingMinor}
}
func (r *divergentRepos) Inbox() service.InboxRepository   { return fakeInbox{} }
func (r *divergentRepos) Outbox() service.OutboxRepository { return fakeOutbox{r.store} }

type divergentLedger struct {
	s            *fakeStore
	openingMinor int64
}

func (r divergentLedger) Append(ctx context.Context, e domain.LedgerEntry) error {
	return fakeLedger{r.s}.Append(ctx, e)
}
func (r divergentLedger) ListByWallet(ctx context.Context, id uuid.UUID, c *service.LedgerCursor, n int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	return ledgerLedger{s: r.s, openingMinor: r.openingMinor}.ListByWallet(ctx, id, c, n)
}
func (r divergentLedger) Reconcile(ctx context.Context, id uuid.UUID) (int64, int64, int64, error) {
	stored, sum, count, err := ledgerLedger{s: r.s, openingMinor: r.openingMinor}.Reconcile(ctx, id)
	return stored, sum + 1, count, err // deliberate off-by-one divergence
}

func TestReconcileMapsMissingWallet(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, err := store.walletService().Reconcile(context.Background(), uuid.NewString())
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
}

// --- transaction lookups ----------------------------------------------------

func TestGetByIDReturnsTransaction(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)
	res, err := svc.Submit(context.Background(), betInput(w))
	require.NoError(t, err)

	got, err := svc.GetByID(context.Background(), res.Transaction.ID().String())
	require.NoError(t, err)
	assert.Equal(t, res.Transaction.ID(), got.ID())
}

func TestGetByIDRejectsMalformedID(t *testing.T) {
	store := newFakeStore(t, "0.00")
	_, err := newWagerService(store).GetByID(context.Background(), "not-a-uuid")
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestGetByIDMapsMissingTransaction(t *testing.T) {
	store := newFakeStore(t, "0.00")
	_, err := newWagerService(store).GetByID(context.Background(), uuid.NewString())
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
	assert.Equal(t, service.CodeTransactionNotFound, service.NotFoundCode(err))
}

func TestGetByProviderExternalRequiresBoth(t *testing.T) {
	store := newFakeStore(t, "0.00")
	_, err := newWagerService(store).GetByProviderExternal(context.Background(), "", "ext")
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestGetByProviderExternalMapsMissing(t *testing.T) {
	store := newFakeStore(t, "0.00")
	_, err := newWagerService(store).GetByProviderExternal(context.Background(), "provider-a", "does-not-exist")
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
}

func TestGetByProviderExternalFindsTransaction(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)
	in := betInput(w)
	res, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	got, err := svc.GetByProviderExternal(context.Background(), in.ProviderID, in.ExternalTransactionID)
	require.NoError(t, err)
	assert.Equal(t, res.Transaction.ID(), got.ID())
}

// --- Prepare / ExecuteInTx (the SQS consumer path) --------------------------

func TestPrepareBuildsTransactionWithoutPersisting(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	tx, err := newWagerService(store).Prepare(betInput(w))
	require.NoError(t, err)
	require.NotNil(t, tx)
	assert.Equal(t, domain.KindBet, tx.Kind())
	assert.Empty(t, store.txByID, "Prepare must not persist")
	assert.Empty(t, store.ledger, "Prepare must not move money")
}

func TestPrepareRejectsBadInput(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	bad := betInput(w)
	bad.IdempotencyKey = ""
	_, err := newWagerService(store).Prepare(bad)
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestExecuteInTxRunsInsideProvidedRepos(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	tx, err := svc.Prepare(betInput(w))
	require.NoError(t, err)

	var res service.WagerResult
	err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
		var e error
		res, e = svc.ExecuteInTx(ctx, r, tx)
		return e
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	assert.Equal(t, "90.00", store.wallet(t).Balance().String())
}

// --- reference resolution (REFUND / ROLLBACK) -------------------------------

// refundInput builds a REFUND of a prior BET. The external id of the reference
// is carried in ReferenceExternalTransactionID; the input's own external id is
// independent.
func refundInput(w *domain.Wallet, refExternalID string, amount string) service.ExternalInput {
	in := betInput(w)
	in.ExternalTransactionID = "refund-" + uuid.NewString()
	in.IdempotencyKey = "refundkey-" + uuid.NewString()
	in.Kind = "REFUND"
	in.Amount = amount
	in.ReferenceExternalTransactionID = refExternalID
	return in
}

func TestRefundWithUnknownReferenceIsPending(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	res, err := newWagerService(store).Submit(context.Background(), refundInput(w, "ghost-ext-id", "10.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPendingReference, res.Transaction.Status(),
		"an unknown reference is a future obligation, not a rejection")
	assert.Equal(t, domain.ZeroMoneyMust("BRL"), res.WalletBalance)
	assert.Equal(t, int64(0), res.WalletVersion)
}

func TestRefundAppliesAgainstProcessedBet(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	betRes, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, betRes.Transaction.Status())
	require.Equal(t, "90.00", store.wallet(t).Balance().String())

	refundRes, err := svc.Submit(context.Background(), refundInput(w, bet.ExternalTransactionID, "10.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, refundRes.Transaction.Status())
	assert.Equal(t, "100.00", store.wallet(t).Balance().String(), "the BET is given back")
	require.Len(t, store.ledger, 2)
	assert.Equal(t, domain.DirectionCredit, store.ledger[1].Direction())
}

func TestRefundRejectsAmountMismatch(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	_, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)

	res, err := svc.Submit(context.Background(), refundInput(w, bet.ExternalTransactionID, "5.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailReferenceAmountMismatch, res.Transaction.FailureCode())
}

func TestRefundTwiceIsRejectedAsDuplicateReversal(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	_, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)
	_, err = svc.Submit(context.Background(), refundInput(w, bet.ExternalTransactionID, "10.00"))
	require.NoError(t, err)

	second, err := svc.Submit(context.Background(), refundInput(w, bet.ExternalTransactionID, "10.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, second.Transaction.Status())
	assert.Equal(t, service.FailDuplicateReversal, second.Transaction.FailureCode())
}

func TestRefundWrongKindIsRejected(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	// Reference exists but is not a BET: build a WIN that references nothing,
	// then a REFUND pointing at the WIN's external id.
	win := betInput(w)
	win.Kind = "WIN"
	win.ExternalTransactionID = "win-" + uuid.NewString()
	_, err := svc.Submit(context.Background(), win)
	require.NoError(t, err)

	res, err := svc.Submit(context.Background(), refundInput(w, win.ExternalTransactionID, "10.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailReferenceKindMismatch, res.Transaction.FailureCode())
}

// --- resolution worker path -------------------------------------------------

func TestResolvePendingAdvancesOnceReferenceArrives(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	// 1. REFUND arrives before its BET -> PENDING_REFERENCE.
	refund := refundInput(w, "late-bet-ext", "10.00")
	pend, err := svc.Submit(context.Background(), refund)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPendingReference, pend.Transaction.Status())

	// 2. The referenced BET finally lands (a real debit).
	late := betInput(w)
	late.ExternalTransactionID = "late-bet-ext"
	_, err = svc.Submit(context.Background(), late)
	require.NoError(t, err)

	// 3. The worker resolves the pending refund against the now-present BET.
	var res service.WagerResult
	err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
		var e error
		res, e = svc.ResolvePending(ctx, r, pend.Transaction)
		return e
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	// 100.00 - 10.00 (bet) + 10.00 (refund) == 100.00
	assert.Equal(t, "100.00", store.wallet(t).Balance().String())
}

func TestResolvePendingRetriesWhenReferenceStillMissing(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	pend, err := svc.Submit(context.Background(), refundInput(w, "never-arrives", "10.00"))
	require.NoError(t, err)
	require.Equal(t, domain.StatusPendingReference, pend.Transaction.Status())

	var res service.WagerResult
	err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
		var e error
		res, e = svc.ResolvePending(ctx, r, pend.Transaction)
		return e
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPendingReference, res.Transaction.Status(), "still waiting")
	assert.Greater(t, res.Transaction.PendingAttempts(), 0, "the retry counter advanced")
	require.NotNil(t, res.Transaction.NextRetryAt())
}

func TestWithReferencePolicyMakesPendingExpire(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	// One max attempt: Submit already consumed it via scheduleReference, so
	// the first resolve (still missing the reference) exhausts the budget and
	// the pending operation is finally rejected.
	svc := service.NewWagerService(store, slog.Default(), store.instrument(),
		service.WithReferencePolicy(1, time.Hour))

	pend, err := svc.Submit(context.Background(), refundInput(w, "never-arrives", "10.00"))
	require.NoError(t, err)
	require.Equal(t, domain.StatusPendingReference, pend.Transaction.Status())

	var res service.WagerResult
	err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
		var e error
		res, e = svc.ResolvePending(ctx, r, pend.Transaction)
		return e
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailReferenceNotFound, res.Transaction.FailureCode())
}

// --- error classification surface -------------------------------------------

func TestErrorTypesExposeStableMessages(t *testing.T) {
	ve := &service.ValidationError{Detail: "bad"}
	assert.Equal(t, "VALIDATION_FAILED: bad", ve.Error())

	nf := &service.NotFoundError{Code: service.CodeWalletNotFound, Detail: "nope"}
	assert.Equal(t, service.CodeWalletNotFound+": nope", nf.Error())

	ce := &service.ConflictError{Code: service.CodeWalletExists, Detail: "dup"}
	assert.Equal(t, service.CodeWalletExists+": dup", ce.Error())

	cause := errors.New("boom")
	ue := &service.UnavailableError{Cause: cause}
	assert.Contains(t, ue.Error(), "boom")
	assert.ErrorIs(t, ue, cause, "UnavailableError unwraps to its cause")

	assert.False(t, service.IsValidation(nf))
	assert.False(t, service.IsUnavailable(ve))
	assert.True(t, service.IsUnavailable(ue))
	assert.True(t, service.IsValidation(ve))
}

func TestSpanAttrAccessors(t *testing.T) {
	a := service.Attr("wallet.kind", "BET")
	assert.Equal(t, "wallet.kind", a.Key)
	assert.Equal(t, "BET", a.Val)
}

// --- LOSS / WIN kinds -------------------------------------------------------

func kindInput(w *domain.Wallet, kind string, amount string) service.ExternalInput {
	in := betInput(w)
	in.ExternalTransactionID = kind + "-" + uuid.NewString()
	in.IdempotencyKey = kind + "-key-" + uuid.NewString()
	in.Kind = kind
	in.Amount = amount
	return in
}

func TestSubmitProcessesLossWithoutMovingMoney(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	res, err := newWagerService(store).Submit(context.Background(), kindInput(w, "LOSS", "0.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	assert.Equal(t, "100.00", store.wallet(t).Balance().String(), "a LOSS moves no money")
	assert.Empty(t, store.ledger, "a LOSS writes no ledger entry")
	require.Len(t, store.outbox, 1, "LOSS emits only the processed event")
}

func TestSubmitProcessesWinCredits(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	res, err := newWagerService(store).Submit(context.Background(), kindInput(w, "WIN", "25.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	assert.Equal(t, "125.00", store.wallet(t).Balance().String())
	require.Len(t, store.ledger, 1)
	assert.Equal(t, domain.DirectionCredit, store.ledger[0].Direction())
}

// TestReplayOfLossReportsCurrentState exercises replayResult's branch where the
// original operation recorded no movement (BalanceAfter is nil): the replay
// must fall back to reading the wallet's current state.
func TestReplayOfLossReportsCurrentState(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	in := kindInput(w, "LOSS", "0.00")
	first, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)

	replay, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay, "same key + payload replays")
	assert.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	assert.Equal(t, "100.00", replay.WalletBalance.String())
}

// TestReplayOfBetCarriesRecordedBalance exercises the other replayResult branch
// (BalanceAfter present): the replay reports the recorded post-movement state,
// not the live balance.
func TestReplayOfBetCarriesRecordedBalance(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	in := betInput(w)
	first, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, "90.00", first.WalletBalance.String())

	replay, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, "90.00", replay.WalletBalance.String(), "the recorded balance, not the live one")
}

// --- reference agreement rejections -----------------------------------------

func TestRollbackRejectsReferenceToAnotherRound(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	res, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, res.Transaction.Status())

	// ROLLBACK of the BET, but stamped with a different round: the reference
	// agreement check must reject it before any money moves.
	rb := kindInput(w, "ROLLBACK", "10.00")
	rb.ReferenceExternalTransactionID = bet.ExternalTransactionID
	rb.RoundID = "another-round"

	out, err := svc.Submit(context.Background(), rb)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, out.Transaction.Status())
	assert.Equal(t, service.FailReferenceMismatch, out.Transaction.FailureCode())
	assert.Equal(t, "90.00", store.wallet(t).Balance().String(), "no reversal applied")
}

func TestRollbackAppliesAgainstProcessedBet(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	_, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)
	require.Equal(t, "90.00", store.wallet(t).Balance().String())

	rb := kindInput(w, "ROLLBACK", "10.00")
	rb.ReferenceExternalTransactionID = bet.ExternalTransactionID
	out, err := svc.Submit(context.Background(), rb)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusProcessed, out.Transaction.Status())
	assert.Equal(t, "100.00", store.wallet(t).Balance().String(), "the BET is rolled back")
}

func TestReversalRejectsCurrencyMismatch(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	_, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)

	rf := kindInput(w, "REFUND", "10.00")
	rf.ReferenceExternalTransactionID = bet.ExternalTransactionID
	rf.Currency = "USD" // differs from the wallet currency
	out, err := svc.Submit(context.Background(), rf)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, out.Transaction.Status())
	assert.Equal(t, service.FailCurrencyMismatch, out.Transaction.FailureCode())
}

// --- duplicate-race recovery ------------------------------------------------

// racyStore simulates a lost insert race: the first transaction attempt always
// loses with ErrDuplicateInsert, while a "winner" transaction is already
// recorded under the same identity. Submit must recover by re-reading the
// winner in a fresh transaction and serving it as an idempotent replay.
type racyStore struct {
	*fakeStore
}

func (r racyStore) Repos() service.Repos { return &racyRepos{store: r.fakeStore} }

func (r racyStore) WithinTx(ctx context.Context, fn func(ctx context.Context, s service.Repos) error) error {
	return fn(ctx, r.Repos())
}

type racyRepos struct{ store *fakeStore }

func (r *racyRepos) Wallets() service.WalletRepository {
	return fakeWallets{r.store}
}
func (r *racyRepos) WalletOpens() service.WalletOpenRepository {
	return fakeWalletOpens{r.store}
}
func (r *racyRepos) Transactions() service.TransactionRepository { return racyTxs{r.store} }
func (r *racyRepos) Ledger() service.LedgerRepository            { return fakeLedger{r.store} }
func (r *racyRepos) Inbox() service.InboxRepository              { return fakeInbox{} }
func (r *racyRepos) Outbox() service.OutboxRepository            { return fakeOutbox{r.store} }

// racyTxs loses every Insert, like a loser of the unique-index race.
type racyTxs struct{ s *fakeStore }

func (r racyTxs) Insert(context.Context, *domain.WagerTransaction) error {
	return service.ErrDuplicateInsert
}
func (r racyTxs) Save(ctx context.Context, t *domain.WagerTransaction) error {
	return fakeTxs{r.s}.Save(ctx, t)
}
func (r racyTxs) GetByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	return fakeTxs{r.s}.GetByID(ctx, id)
}
func (r racyTxs) GetByProviderAndKey(ctx context.Context, p, k string) (*domain.WagerTransaction, error) {
	return fakeTxs{r.s}.GetByProviderAndKey(ctx, p, k)
}
func (r racyTxs) GetByProviderAndExternal(ctx context.Context, p, e string) (*domain.WagerTransaction, error) {
	return fakeTxs{r.s}.GetByProviderAndExternal(ctx, p, e)
}
func (r racyTxs) ExistsProcessedReversal(ctx context.Context, id uuid.UUID, k domain.TxKind) (bool, error) {
	return fakeTxs{r.s}.ExistsProcessedReversal(ctx, id, k)
}
func (r racyTxs) ClaimPendingReferences(ctx context.Context, t time.Time, n int) ([]*domain.WagerTransaction, error) {
	return fakeTxs{r.s}.ClaimPendingReferences(ctx, t, n)
}

// TestSubmitRecoversFromDuplicateInsertRace builds the exact race: a winner
// transaction with the same (provider, key) and payload already exists, and
// our own Insert loses. Submit must return the winner as an idempotent replay.
func TestSubmitRecoversFromDuplicateInsertRace(t *testing.T) {
	base := newFakeStore(t, "100.00")
	w := base.wallet(t)
	svc := newWagerService(base)
	in := betInput(w)

	// First submit persists the winner via the normal path.
	winner, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	require.False(t, winner.IdempotentReplay)

	// Now the same submission against a racy store: its Insert is rejected, so
	// Submit must fall into duplicateRaceReplay and return the winner.
	racy := racyStore{base}
	recovered, err := service.NewWagerService(racy, slog.Default(), base.instrument()).Submit(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, recovered.IdempotentReplay)
	assert.Equal(t, winner.Transaction.ID(), recovered.Transaction.ID())
	assert.Equal(t, winner.WalletBalance.String(), recovered.WalletBalance.String())
}

// --- replayResult: wallet vanished -----------------------------------------

// TestReplayOfPendingReferenceWithNoWallet covers replayResult's ErrRepoNotFound
// branch: relaying a PENDING_REFERENCE whose wallet no longer resolves reports a
// zero balance instead of a hard error.
func TestReplayOfPendingReferenceWithNoWallet(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	in := refundInput(w, "ghost", "10.00")
	first, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPendingReference, first.Transaction.Status())

	// Remove the wallet from the store so the replay's fallback lookup misses.
	store.mu.Lock()
	delete(store.wallets, w.ID())
	store.mu.Unlock()

	replay, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, replay.IdempotentReplay)
	assert.Equal(t, "0.00", replay.WalletBalance.String())
}

// --- reversal against insufficient funds ------------------------------------

func TestRollbackOfWinBeyondBalanceIsRejected(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	// WIN 50.00 -> 150.00, then BET 120.00 -> 30.00. Rolling back the WIN must
	// debit 50.00 but only 30.00 remains: REVERSAL_INSUFFICIENT_FUNDS.
	win := kindInput(w, "WIN", "50.00")
	winRes, err := svc.Submit(context.Background(), win)
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, winRes.Transaction.Status())

	bet := betInput(w)
	bet.Amount = "120.00"
	_, err = svc.Submit(context.Background(), bet)
	require.NoError(t, err)
	require.Equal(t, "30.00", store.wallet(t).Balance().String())

	rb := kindInput(w, "ROLLBACK", "50.00")
	rb.ReferenceExternalTransactionID = win.ExternalTransactionID
	out, err := svc.Submit(context.Background(), rb)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, out.Transaction.Status())
	assert.Equal(t, service.FailReversalInsufficientFunds, out.Transaction.FailureCode())
}

// --- resolve: wallet vanished ----------------------------------------------

func TestResolvePendingWhenWalletDisappeared(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	pend, err := svc.Submit(context.Background(), refundInput(w, "ghost", "10.00"))
	require.NoError(t, err)
	require.Equal(t, domain.StatusPendingReference, pend.Transaction.Status())

	store.mu.Lock()
	delete(store.wallets, w.ID())
	store.mu.Unlock()

	var res service.WagerResult
	err = store.WithinTx(context.Background(), func(ctx context.Context, r service.Repos) error {
		var e error
		res, e = svc.ResolvePending(ctx, r, pend.Transaction)
		return e
	})
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailReferenceNotFound, res.Transaction.FailureCode())
}

// --- open: duplicate wallet for the same player/currency --------------------

func TestOpenRejectsSecondWalletForSamePlayer(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	svc := store.walletService()

	playerID := uuid.NewString()
	_, err := svc.Open(context.Background(), openInput(playerID, "10.00"))
	require.NoError(t, err)

	// A second OPENING for the same player+currency must conflict, not create
	// a second wallet.
	_, err = svc.Open(context.Background(), openInput(playerID, "5.00"))
	require.Error(t, err)
	assert.True(t, service.IsConflict(err))
	assert.Equal(t, service.CodeWalletExists, service.ConflictCode(err))
}

func TestOpenRejectsInvalidPlayerID(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, err := store.walletService().Open(context.Background(), service.OpenWalletInput{
		PlayerID: "not-a-uuid", InitialAmount: "10.00", Currency: "BRL",
	})
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

func TestOpenRejectsMissingCurrency(t *testing.T) {
	store := newLedgerStore(t, "0.00")
	_, err := store.walletService().Open(context.Background(), service.OpenWalletInput{
		PlayerID: uuid.NewString(), InitialAmount: "10.00",
	})
	require.Error(t, err)
	assert.True(t, service.IsValidation(err))
}

// --- reference agreement: player mismatch -----------------------------------

func TestReversalRejectsReferenceToAnotherPlayer(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)
	svc := newWagerService(store)

	bet := betInput(w)
	_, err := svc.Submit(context.Background(), bet)
	require.NoError(t, err)

	// Same wallet and round, but the reversal claims another player: the
	// wallet's ownership check rejects it before the reference agreement.
	rf := refundInput(w, bet.ExternalTransactionID, "10.00")
	rf.PlayerID = uuid.NewString()
	out, err := svc.Submit(context.Background(), rf)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusRejected, out.Transaction.Status())
	assert.Equal(t, service.FailPlayerWalletMismatch, out.Transaction.FailureCode())
}
