package messaging

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// --- reconcile worker harness ----------------------------------------------

// reconStore is an in-memory store with just enough shape for
// WalletService.Reconcile: a set of wallets and a stored-vs-ledger balance
// pair per wallet. "ledgerSum" differing from the wallet's balance is exactly
// the divergence the sweep must catch.
type reconStore struct {
	mu      sync.Mutex
	wallets map[uuid.UUID]*domain.Wallet
	ledger  map[uuid.UUID]int64 // ledger-derived sum, per wallet
	entries map[uuid.UUID]int64 // entry count, per wallet
	// listOverride, when non-nil, is returned by ListIDs instead of the real
	// key set. It lets a test hand the sweep an id that no longer resolves,
	// reproducing the delete-between-list-and-check race deterministically.
	listOverride []uuid.UUID
}

func newReconStore() *reconStore {
	return &reconStore{
		wallets: map[uuid.UUID]*domain.Wallet{},
		ledger:  map[uuid.UUID]int64{},
		entries: map[uuid.UUID]int64{},
	}
}

// add registers a wallet with a stored balance and the ledger sum to compare
// it against. The two are equal unless the caller wants a divergence.
func (s *reconStore) add(balanceMinor, ledgerMinor int64) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	initial, err := domain.NewMoney(balanceMinor, "BRL")
	if err != nil {
		panic(err)
	}
	w, _, err := domain.OpenWallet(uuid.New(), "BRL", initial, time.Now())
	if err != nil {
		panic(err)
	}
	id := w.ID()
	s.wallets[id] = w
	s.ledger[id] = ledgerMinor
	s.entries[id] = 1
	return id
}

type reconTxm struct{ s *reconStore }

func (t reconTxm) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	return fn(ctx, reconRepos{t.s})
}
func (t reconTxm) Repos() service.Repos { return reconRepos{t.s} }

type reconRepos struct{ s *reconStore }

func (r reconRepos) Wallets() service.WalletRepository           { return reconWallets{r.s} }
func (r reconRepos) Ledger() service.LedgerRepository            { return reconLedger{r.s} }
func (r reconRepos) Postings() service.PostingRepository         { return nil }
func (r reconRepos) WalletOpens() service.WalletOpenRepository   { return nil }
func (r reconRepos) Transactions() service.TransactionRepository { return nil }
func (r reconRepos) Inbox() service.InboxRepository              { return nil }
func (r reconRepos) Outbox() service.OutboxRepository            { return nil }

type reconWallets struct{ s *reconStore }

func (r reconWallets) Create(context.Context, *domain.Wallet) error        { return nil }
func (r reconWallets) UpdateBalance(context.Context, *domain.Wallet) error { return nil }
func (r reconWallets) GetForUpdate(context.Context, uuid.UUID) (*domain.Wallet, error) {
	return nil, service.ErrRepoNotFound
}
func (r reconWallets) GetByPlayerAndCurrency(context.Context, uuid.UUID, string, bool) (*domain.Wallet, error) {
	return nil, service.ErrRepoNotFound
}
func (r reconWallets) GetByID(_ context.Context, id uuid.UUID) (*domain.Wallet, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.wallets[id]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return w, nil
}

// ListIDs mirrors the SQL keyset contract (ascending, strictly-after cursor,
// bounded) so the sweep's pagination loop is exercised the way production
// paginates.
func (r reconWallets) ListIDs(_ context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.listOverride != nil {
		if after == nil {
			return append([]uuid.UUID(nil), r.s.listOverride...), nil
		}
		return nil, nil // one page is enough for the override scenario
	}
	var ids []uuid.UUID
	for id := range r.s.wallets {
		if after == nil || id.String() > after.String() {
			ids = append(ids, id)
		}
	}
	sortUUIDs(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

type reconLedger struct{ s *reconStore }

func (r reconLedger) Append(context.Context, domain.LedgerEntry) error { return nil }
func (r reconLedger) ListByWallet(context.Context, uuid.UUID, *service.LedgerCursor, int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	return nil, nil, nil
}
func (r reconLedger) Reconcile(_ context.Context, id uuid.UUID) (stored, sum, count int64, err error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.wallets[id]
	if !ok {
		return 0, 0, 0, service.ErrRepoNotFound
	}
	return w.Balance().AmountMinor(), r.s.ledger[id], r.s.entries[id], nil
}

func sortUUIDs(ids []uuid.UUID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j].String() < ids[j-1].String(); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func reconConfig(batch int) *config.Config {
	return &config.Config{ReconcileInterval: 10 * time.Millisecond, ReconcileBatch: batch}
}

// sweepingHook is the messaging.Instrumentation the worker observes through.
// Per the port, the sweep reports only how many wallets it checked; the
// divergences are counted by the use case through ReconciliationDivergence,
// which svcHook routes into the same counter — so the tally below reflects the
// single real source, exactly as production wires it.
type sweepingHook struct {
	mu         sync.Mutex
	checked    int
	divergent  int
	sweepCount int
}

func (h *sweepingHook) ReconciliationSweep(checked int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepCount++
	h.checked += checked
}

func (h *sweepingHook) noteDivergence() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.divergent++
}

func (h *sweepingHook) OutboxPublished()           {}
func (h *sweepingHook) OutboxFailed()              {}
func (h *sweepingHook) OutboxStats(int64, float64) {}
func (h *sweepingHook) StartSpan(ctx context.Context, _ string, _ ...SpanAttr) (context.Context, Span) {
	return ctx, noopSpan{}
}

// svcHook satisfies service.Instrumentation (the use case's port). It embeds
// sweepingHook to share the sweep counters and redeclares StartSpan: the two
// ports carry different attribute types, and Go resolves the outer method, so
// one value serves the service while the embedded one serves the worker.
type svcHook struct{ *sweepingHook }

func (svcHook) WagerOutcome(string, string, string, string) {}
func (svcHook) Duplicate(string)                            {}
func (svcHook) ReferenceOutcome(string)                     {}

// ReconciliationDivergence is the use case reporting a mismatch — the single
// counter the metric is built on. It routes into the shared tally so a sweep
// that finds the planted divergence registers exactly one, per sweep.
func (h svcHook) ReconciliationDivergence() { h.sweepingHook.noteDivergence() }
func (svcHook) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, svcNoopSpan{}
}

// svcNoopSpan is the no-op service.Span, distinct from messaging's noopSpan
// because the two ports carry different attribute types.
type svcNoopSpan struct{}

func (svcNoopSpan) SetAttributes(...service.SpanAttr) {}
func (svcNoopSpan) SetOutcome(string, string)         {}
func (svcNoopSpan) RecordError(error)                 {}
func (svcNoopSpan) End()                              {}

func newReconcileWorker(t *testing.T, store *reconStore, batch int) (*ReconcileWorker, *sweepingHook) {
	t.Helper()
	hook := &sweepingHook{}
	svc := service.NewWalletService(reconTxm{store}, slog.Default(), svcHook{hook})
	return NewReconcileWorker(reconTxm{store}, svc, reconConfig(batch), slog.Default(), hook), hook
}

// --- tests ------------------------------------------------------------------

// TestReconcileSweepChecksEveryWalletInBatches: with more wallets than one
// batch holds, the sweep must paginate through all of them — the whole point
// of a sweep is that no wallet is skipped.
func TestReconcileSweepChecksEveryWalletInBatches(t *testing.T) {
	store := newReconStore()
	const n = 25
	for i := 0; i < n; i++ {
		store.add(1000, 1000) // consistent
	}
	w, hook := newReconcileWorker(t, store, 10) // batch < n forces pagination

	w.sweep(context.Background())

	assert.Equal(t, n, hook.checked, "every wallet must be visited across the pages")
	assert.Equal(t, 0, hook.divergent)
	assert.Equal(t, 1, hook.sweepCount, "one pass is one sweep")
}

// TestReconcileSweepCountsDivergences: a wallet whose stored balance no longer
// matches its ledger must be counted, and the count must survive the batching.
func TestReconcileSweepCountsDivergences(t *testing.T) {
	store := newReconStore()
	for i := 0; i < 5; i++ {
		store.add(1000, 1000)
	}
	store.add(1000, 700) // stored 1000, ledger 700 -> divergent
	store.add(50, 900)   // stored 50, ledger 900 -> divergent

	w, hook := newReconcileWorker(t, store, 100)

	w.sweep(context.Background())

	assert.Equal(t, 7, hook.checked)
	assert.Equal(t, 2, hook.divergent)
}

// TestReconcileSweepOnEmptyLedgerTableIsClean: nothing to check is not a
// failure, and it still counts as one completed sweep (so a stalled worker is
// visible as "no sweeps", not "no divergences").
func TestReconcileSweepOnEmptyTableIsClean(t *testing.T) {
	store := newReconStore()
	w, hook := newReconcileWorker(t, store, 10)

	assert.NotPanics(t, func() { w.sweep(context.Background()) })
	assert.Equal(t, 0, hook.checked)
	assert.Equal(t, 1, hook.sweepCount)
}

// TestReconcileSweepStopsOnCancelledContext: a shutdown mid-pass must return
// promptly instead of walking the rest of the table.
func TestReconcileSweepStopsOnCancelledContext(t *testing.T) {
	store := newReconStore()
	for i := 0; i < 50; i++ {
		store.add(1000, 1000)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w, _ := newReconcileWorker(t, store, 10)
	done := make(chan struct{})
	go func() { w.sweep(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep ignored context cancellation")
	}
}

// TestReconcileSweepToleratesVanishedWallet: a wallet deleted between the page
// listing and its check must be skipped, not counted, and must not abort the
// pass. This drives the real race: the store lists an id that Reconcile then
// cannot find. Reconcile maps the repository's ErrRepoNotFound into a
// WALLET_NOT_FOUND NotFoundError, and that code — not the repo sentinel — is
// what the worker must recognise as benign.
func TestReconcileSweepToleratesVanishedWallet(t *testing.T) {
	store := newReconStore()
	survivor := store.add(1000, 1000)
	vanished := uuid.New() // never inserted: the id a page can still hand back

	w, hook := newReconcileWorker(t, store, 10)
	_ = survivor

	// The listing claims both ids exist; only one does. This is exactly the
	// window between ListIDs and GetByID under concurrent deletion.
	store.mu.Lock()
	store.listOverride = []uuid.UUID{vanished, survivor}
	store.mu.Unlock()

	// The use case surfaces an unknown wallet as a coded NotFoundError.
	_, err := service.NewWalletService(reconTxm{store}, slog.Default(), svcHook{hook}).Reconcile(context.Background(), vanished.String())
	require.Equal(t, service.CodeWalletNotFound, service.NotFoundCode(err),
		"an unknown wallet must surface as WALLET_NOT_FOUND, which the sweep skips")

	assert.NotPanics(t, func() { w.sweep(context.Background()) })
	assert.Equal(t, 1, hook.checked, "only the surviving wallet counts")
	assert.Equal(t, 0, hook.divergent, "a vanished wallet is a lifecycle race, not a divergence")
}

// TestReconcileRunStopsOnContextCancel mirrors the other workers' shutdown.
func TestReconcileRunStopsOnContextCancel(t *testing.T) {
	store := newReconStore()
	store.add(1000, 1000)
	w, _ := newReconcileWorker(t, store, 10)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile Run did not return after cancellation")
	}
}

// errListIDs proves a listing failure is logged and ends the pass instead of
// looping forever.
type errListIDs struct{ reconWallets }

func (errListIDs) ListIDs(context.Context, *uuid.UUID, int) ([]uuid.UUID, error) {
	return nil, errors.New("listing down")
}

type errListRepos struct{ reconRepos }

func (r errListRepos) Wallets() service.WalletRepository {
	return errListIDs{reconWallets{r.reconRepos.s}}
}

type errListTxm struct{ s *reconStore }

func (t errListTxm) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	return fn(ctx, errListRepos{reconRepos{t.s}})
}
func (t errListTxm) Repos() service.Repos { return errListRepos{reconRepos{t.s}} }

func TestReconcileSweepSurvivesListFailure(t *testing.T) {
	store := newReconStore()
	hook := &sweepingHook{}
	svc := service.NewWalletService(reconTxm{store}, slog.Default(), svcHook{hook})
	w := NewReconcileWorker(errListTxm{store}, svc, reconConfig(10), slog.Default(), hook)

	assert.NotPanics(t, func() { w.sweep(context.Background()) })
	assert.Equal(t, 1, hook.sweepCount, "a failed pass is still accounted as one sweep")
	assert.Equal(t, 0, hook.checked)
}
