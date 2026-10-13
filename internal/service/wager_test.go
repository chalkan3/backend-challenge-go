// Package service_test exercises the WagerService use case against an
// in-memory implementation of the repository ports (test doubles), keeping
// the unit suite free of PostgreSQL while pinning the business rules:
// idempotent replay, payload conflicts, persisted rejections and the
// financial effects on wallet, ledger and outbox.
package service_test

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// --- fakes ------------------------------------------------------------------
//
// One in-memory state container (fakeStore) plus one small adapter per
// repository port, mirroring how the PostgreSQL adapter bundles them.

type fakeStore struct {
	mu       sync.Mutex
	wallets  map[uuid.UUID]*domain.Wallet
	txByID   map[uuid.UUID]*domain.WagerTransaction
	txByKey  map[string]*domain.WagerTransaction // provider|idempotencyKey
	txByExt  map[string]*domain.WagerTransaction // provider|externalTransactionId
	ledger   []domain.LedgerEntry
	journal  []domain.JournalEntry
	outbox   []domain.Envelope
	opens    map[string]fakeOpen // idempotencyKey -> recorded opening
	outcomes []string            // recorded service.Instrumentation.WagerOutcome lines
}

// fakeOpen is a recorded wallet-opening idempotency row.
type fakeOpen struct {
	hash     string
	walletID uuid.UUID
}

func newFakeStore(t *testing.T, opening string) *fakeStore {
	t.Helper()
	w, _, err := domain.OpenWallet(domain.MustNewID(), "BRL", mustMoney(t, opening, "BRL"), time.Now().UTC())
	require.NoError(t, err)
	return &fakeStore{
		wallets: map[uuid.UUID]*domain.Wallet{w.ID(): w},
		txByID:  map[uuid.UUID]*domain.WagerTransaction{},
		txByKey: map[string]*domain.WagerTransaction{},
		txByExt: map[string]*domain.WagerTransaction{},
		opens:   map[string]fakeOpen{},
	}
}

func (f *fakeStore) wallet(t *testing.T) *domain.Wallet {
	t.Helper()
	for _, w := range f.wallets {
		return w
	}
	t.Fatal("no wallet in store")
	return nil
}

// WithinTx runs the callback immediately: the fake has no isolation, which
// is enough to pin the use case's business rules in unit tests.
func (f *fakeStore) WithinTx(ctx context.Context, fn func(ctx context.Context, r service.Repos) error) error {
	return fn(ctx, f.Repos())
}

func (f *fakeStore) Repos() service.Repos { return &fakeRepos{store: f} }

// instrument returns the store itself as the observation port, recording
// every outcome for assertions (spy).
func (f *fakeStore) instrument() service.Instrumentation { return f }

func (f *fakeStore) WagerOutcome(kind, status, failureCode, channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, strings.Join([]string{kind, status, failureCode, channel}, "/"))
}
func (f *fakeStore) Duplicate(string)          {}
func (f *fakeStore) ReferenceOutcome(string)   {}
func (f *fakeStore) ReconciliationDivergence() {}

// StartSpan returns a recording no-op span: the use cases must work with any
// tracer, including one that drops every span (telemetry disabled).
func (f *fakeStore) StartSpan(ctx context.Context, name string, attrs ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, &recordingSpan{name: name}
}

// recordingSpan captures what the use case annotated, so tests can assert on
// the observability contract without a real exporter.
type recordingSpan struct {
	name     string
	outcome  string
	code     string
	failures []error
}

func (s *recordingSpan) SetAttributes(...service.SpanAttr) {}
func (s *recordingSpan) SetOutcome(status, code string)    { s.outcome, s.code = status, code }
func (s *recordingSpan) RecordError(err error) {
	if err != nil {
		s.failures = append(s.failures, err)
	}
}
func (s *recordingSpan) End() {}

// fakeRepos bundles the per-port adapters over one store.
type fakeRepos struct{ store *fakeStore }

func (r *fakeRepos) Wallets() service.WalletRepository           { return fakeWallets{r.store} }
func (r *fakeRepos) WalletOpens() service.WalletOpenRepository   { return fakeWalletOpens{r.store} }
func (r *fakeRepos) Transactions() service.TransactionRepository { return fakeTxs{r.store} }
func (r *fakeRepos) Ledger() service.LedgerRepository            { return fakeLedger{r.store} }
func (r *fakeRepos) Postings() service.PostingRepository         { return fakePostings{r.store} }
func (r *fakeRepos) Inbox() service.InboxRepository              { return fakeInbox{} }
func (r *fakeRepos) Outbox() service.OutboxRepository            { return fakeOutbox{r.store} }

type fakeWallets struct{ s *fakeStore }

func (r fakeWallets) Create(_ context.Context, w *domain.Wallet) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.wallets[w.ID()] = w
	return nil
}

func (r fakeWallets) UpdateBalance(_ context.Context, w *domain.Wallet) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.wallets[w.ID()] = w // shared aggregate pointer
	return nil
}

func (r fakeWallets) GetByID(_ context.Context, id uuid.UUID) (*domain.Wallet, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.wallets[id]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return w, nil
}

func (r fakeWallets) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	return r.GetByID(ctx, id)
}

func (r fakeWallets) GetByPlayerAndCurrency(_ context.Context, playerID uuid.UUID, currency string, _ bool) (*domain.Wallet, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, w := range r.s.wallets {
		if w.PlayerID() == playerID && w.Currency() == currency {
			return w, nil
		}
	}
	return nil, service.ErrRepoNotFound
}

// ListIDs mirrors the SQL keyset contract: ids strictly greater than after,
// ascending, at most limit. Ordering is explicit so the fake has the same
// total order as the ORDER BY id ASC it stands in for (map iteration order
// is random).
func (r fakeWallets) ListIDs(_ context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var ids []uuid.UUID
	for id := range r.s.wallets {
		if after == nil || id.String() > after.String() {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// fakeWalletOpens records wallet-opening idempotency keys in memory.
type fakeWalletOpens struct{ s *fakeStore }

func (r fakeWalletOpens) Record(_ context.Context, key, payloadHash string, walletID uuid.UUID) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, exists := r.s.opens[key]; exists {
		return service.ErrDuplicateInsert
	}
	r.s.opens[key] = fakeOpen{hash: payloadHash, walletID: walletID}
	return nil
}

func (r fakeWalletOpens) Lookup(_ context.Context, key string) (uuid.UUID, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	o, ok := r.s.opens[key]
	if !ok {
		return uuid.Nil, "", service.ErrRepoNotFound
	}
	return o.walletID, o.hash, nil
}

type fakeTxs struct{ s *fakeStore }

func (r fakeTxs) Insert(_ context.Context, t *domain.WagerTransaction) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, exists := r.s.txByID[t.ID()]; exists {
		return service.ErrDuplicateInsert
	}
	r.s.txByID[t.ID()] = t
	r.s.txByKey[t.ProviderID()+"|"+t.IdempotencyKey()] = t
	r.s.txByExt[t.ProviderID()+"|"+t.ExternalTxID()] = t
	return nil
}

func (r fakeTxs) Save(_ context.Context, t *domain.WagerTransaction) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.txByID[t.ID()] = t
	return nil
}

func (r fakeTxs) GetByID(_ context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	t, ok := r.s.txByID[id]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return t, nil
}

func (r fakeTxs) GetByProviderAndKey(_ context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	t, ok := r.s.txByKey[providerID+"|"+key]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return t, nil
}

func (r fakeTxs) GetByProviderAndExternal(_ context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	t, ok := r.s.txByExt[providerID+"|"+externalID]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return t, nil
}

func (r fakeTxs) ExistsProcessedReversal(_ context.Context, referenceTxID uuid.UUID, kind domain.TxKind) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, t := range r.s.txByID {
		ref := t.ReferenceTxID()
		if t.Status() == domain.StatusProcessed && t.Kind() == kind && ref != nil && *ref == referenceTxID {
			return true, nil
		}
	}
	return false, nil
}

func (r fakeTxs) ClaimPendingReferences(context.Context, time.Time, int) ([]*domain.WagerTransaction, error) {
	return nil, nil
}

type fakeLedger struct{ s *fakeStore }

func (r fakeLedger) Append(_ context.Context, e domain.LedgerEntry) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.ledger = append(r.s.ledger, e)
	return nil
}

func (r fakeLedger) ListByWallet(context.Context, uuid.UUID, *service.LedgerCursor, int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	return nil, nil, nil
}

func (r fakeLedger) Reconcile(context.Context, uuid.UUID) (int64, int64, int64, error) {
	return 0, 0, 0, nil
}

// fakePostings records journal entries so tests can assert the second leg was
// written, and reads them back so a rehydrated entry can be compared.
type fakePostings struct{ s *fakeStore }

func (r fakePostings) Append(_ context.Context, e domain.JournalEntry) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.journal = append(r.s.journal, e)
	return nil
}

type fakeInbox struct{}

func (fakeInbox) Begin(context.Context, string, string, string) (service.InboxOutcome, error) {
	return service.InboxOutcome{Inserted: true}, nil
}
func (fakeInbox) Complete(context.Context, string, string, time.Time) error { return nil }

type fakeOutbox struct{ s *fakeStore }

func (r fakeOutbox) Enqueue(_ context.Context, envelopes []domain.Envelope) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.outbox = append(r.s.outbox, envelopes...)
	return nil
}

func (fakeOutbox) ClaimBatch(context.Context, time.Time, time.Duration, int) ([]service.OutboxItem, error) {
	return nil, nil
}

func (fakeOutbox) MarkPublished(context.Context, uuid.UUID, time.Time) error { return nil }
func (fakeOutbox) MarkFailed(context.Context, uuid.UUID, time.Time, string) error {
	return nil
}
func (fakeOutbox) Stats(context.Context) (int64, float64, error) { return 0, 0, nil }

// --- helpers ----------------------------------------------------------------

func betInput(w *domain.Wallet) service.ExternalInput {
	return service.ExternalInput{
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-" + uuid.NewString(),
		IdempotencyKey:        "key-" + uuid.NewString(),
		PlayerID:              w.PlayerID().String(),
		WalletID:              w.ID().String(),
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Amount:                "10.00",
		Currency:              "BRL",
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "corr-test",
	}
}

func newWagerService(store *fakeStore) *service.WagerService {
	return service.NewWagerService(store, slog.Default(), store.instrument())
}

// --- tests ------------------------------------------------------------------

func TestSubmitProcessesBet(t *testing.T) {
	store := newFakeStore(t, "100.00")
	in := betInput(store.wallet(t))

	res, err := newWagerService(store).Submit(context.Background(), in)
	require.NoError(t, err)

	assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
	assert.False(t, res.IdempotentReplay)
	assert.Equal(t, "90.00", res.WalletBalance.String(), "100.00 - 10.00")
	assert.Equal(t, int64(2), res.WalletVersion)
	assert.Equal(t, "90.00", store.wallet(t).Balance().String(), "wallet balance is updated")

	require.Len(t, store.ledger, 1, "one ledger entry for the debit")
	assert.Equal(t, domain.DirectionDebit, store.ledger[0].Direction())
	assert.Equal(t, "10.00", store.ledger[0].Amount().String())

	require.Len(t, store.outbox, 2, "processed + balance-changed events")
	assert.Equal(t, domain.EventWagerTransactionProcessed, store.outbox[0].EventType)
	assert.Equal(t, domain.EventWalletBalanceChanged, store.outbox[1].EventType)

	require.NotEmpty(t, store.outcomes, "instrumentation reports the outcome")
	assert.Contains(t, store.outcomes[0], "BET/PROCESSED/")
}

func TestSubmitReplaysSameIdempotencyKey(t *testing.T) {
	store := newFakeStore(t, "100.00")
	in := betInput(store.wallet(t))
	svc := newWagerService(store)

	first, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	// The wallet moves on with an unrelated WIN.
	win := betInput(store.wallet(t))
	win.Kind, win.Amount = "WIN", "5.00"
	_, err = svc.Submit(context.Background(), win)
	require.NoError(t, err)

	second, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.Equal(t, first.Transaction.ID(), second.Transaction.ID())
	assert.Equal(t, first.Transaction.Status(), second.Transaction.Status())
	assert.Equal(t, "90.00", second.WalletBalance.String(),
		"replay returns the balance observed at the original processing, not the current one")
	assert.Len(t, store.ledger, 2, "no extra ledger entry for the replay")
}

func TestSubmitRejectsKeyReuseWithDifferentPayload(t *testing.T) {
	store := newFakeStore(t, "100.00")
	in := betInput(store.wallet(t))
	svc := newWagerService(store)

	_, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	in.Amount = "99.00"
	_, err = svc.Submit(context.Background(), in)
	require.Error(t, err)
	assert.True(t, service.IsConflict(err))
	assert.Equal(t, service.CodeIdemKeyConflict, service.ConflictCode(err))
}

func TestSubmitRejectsExternalIDUnderDifferentKey(t *testing.T) {
	store := newFakeStore(t, "100.00")
	in := betInput(store.wallet(t))
	svc := newWagerService(store)

	_, err := svc.Submit(context.Background(), in)
	require.NoError(t, err)

	in.IdempotencyKey = "other-key"
	_, err = svc.Submit(context.Background(), in)
	require.Error(t, err)
	assert.True(t, service.IsConflict(err))
	assert.Equal(t, service.CodeExternalIDConflict, service.ConflictCode(err))
}

func TestSubmitPersistsInsufficientFundsRejection(t *testing.T) {
	store := newFakeStore(t, "10.00")
	in := betInput(store.wallet(t))
	in.Amount = "1000.00"

	res, err := newWagerService(store).Submit(context.Background(), in)
	require.NoError(t, err, "a business rejection is a result, not an infrastructure error")

	assert.Equal(t, domain.StatusRejected, res.Transaction.Status())
	assert.Equal(t, service.FailInsufficientFunds, res.Transaction.FailureCode())
	assert.Equal(t, "10.00", store.wallet(t).Balance().String(), "balance untouched")
	assert.Empty(t, store.ledger, "no ledger entry for a rejected operation")
	require.Len(t, store.outbox, 1, "the rejection is audited through the outbox")
	assert.Equal(t, domain.EventWagerTransactionRejected, store.outbox[0].EventType)
}

func TestSubmitWalletNotFound(t *testing.T) {
	store := newFakeStore(t, "10.00")
	in := betInput(store.wallet(t))
	in.WalletID = uuid.NewString()

	_, err := newWagerService(store).Submit(context.Background(), in)
	require.Error(t, err)
	assert.True(t, service.IsNotFound(err))
	assert.Equal(t, service.CodeWalletNotFound, service.NotFoundCode(err))
}

func TestSubmitIsChannelAgnostic(t *testing.T) {
	for _, via := range []domain.ReceivedVia{domain.ViaHTTP, domain.ViaSQS} {
		store := newFakeStore(t, "100.00")
		in := betInput(store.wallet(t))
		in.ReceivedVia = via

		res, err := newWagerService(store).Submit(context.Background(), in)
		require.NoError(t, err, "channel %s", via)
		assert.Equal(t, domain.StatusProcessed, res.Transaction.Status())
		assert.Equal(t, via, res.Transaction.ReceivedVia())
	}
}
