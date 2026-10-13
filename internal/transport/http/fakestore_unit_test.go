// In-package fakes for the endpoint bodies.
//
// endpoints.go is thin glue: it adapts a request struct into a use-case input,
// calls the use case and shapes the response struct. The full use-case
// behaviour (idempotency, locking, ledger arithmetic) is proven against real
// PostgreSQL in test/integration; what is left to pin here is precisely the
// glue — that a missing balance is omitted, that a forbidden transaction is
// refused, that every field of the response is mapped.
//
// The fakes are deliberately small: one state container plus one adapter per
// port, each implementing a single repository interface. Only the ports the
// read paths reach are implemented; the rest return a typed nil so a surprise
// dependency fails loudly instead of passing silently.
package http

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// fakeStore is a tiny in-memory TxManager: one wallet, a list of ledger
// entries and a map of transactions, plus one sentinel error per port so the
// error path of each endpoint can be exercised without a database.
//
// It also carries enough of the write path (Insert, UpdateBalance, Append,
// Enqueue) for a successful submission to run to completion, so the submit
// endpoint's response builder — the mapping from a WagerResult to the JSON
// body — is exercised with a real result rather than a hand-made struct.
type fakeStore struct {
	wallet       *domain.Wallet
	entries      []*domain.LedgerEntry
	txs          map[uuid.UUID]*domain.WagerTransaction
	txsByExt     map[string]*domain.WagerTransaction // provider|external
	txsByKey     map[string]*domain.WagerTransaction // provider|key
	openKeys     map[string]uuid.UUID                // opening idempotency key -> wallet
	outbox       []domain.Envelope
	postings     []domain.JournalEntry
	walletErr    error
	ledgerErr    error
	txErr        error
	reconcileFix int64 // added to the ledger sum, to force divergence
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	w, err := domain.RehydrateWallet(uuid.New(), uuid.New(), "BRL", 5000, 3, now, now)
	if err != nil {
		t.Fatalf("rehydrate wallet: %v", err)
	}
	mov100, _ := domain.NewMoney(1000, "BRL")
	before, _ := domain.NewMoney(4000, "BRL")
	after, _ := domain.NewMoney(5000, "BRL")
	entry, err := domain.RehydrateLedgerEntry(domain.LedgerEntryParams{
		ID: uuid.New(), WalletID: w.ID(), TransactionID: uuid.New(),
		Direction: domain.DirectionCredit, Amount: mov100,
		BalanceBefore: before, BalanceAfter: after, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("rehydrate ledger entry: %v", err)
	}
	return &fakeStore{
		wallet:   w,
		entries:  []*domain.LedgerEntry{entry},
		txs:      map[uuid.UUID]*domain.WagerTransaction{},
		txsByExt: map[string]*domain.WagerTransaction{},
		txsByKey: map[string]*domain.WagerTransaction{},
		openKeys: map[string]uuid.UUID{},
	}
}

// addTx inserts a rehydratable BET transaction owned by providerID.
func (f *fakeStore) addTx(providerID string) *domain.WagerTransaction {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	p := providerID
	bb, ba, ver := int64(5000), int64(4000), int64(4)
	t, err := domain.RehydrateWagerTransaction(domain.RehydrateParams{
		ID: uuid.New(), Kind: domain.KindBet, Origin: domain.OriginExternal,
		ProviderID: &p, ExternalTxID: strPtr("ext-1"), IdempotencyKey: strPtr("key-1"),
		PlayerID: uuid.New(), WalletID: f.wallet.ID(), Currency: "BRL",
		AmountMinor: 1000, Status: domain.StatusProcessed,
		BalanceBeforeMinor: &bb, BalanceAfterMinor: &ba, WalletVersionAfter: &ver,
		ReceivedVia: domain.ViaHTTP, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		panic(fmt.Sprintf("rehydrate transaction: %v", err))
	}
	f.txs[t.ID()] = t
	f.txsByExt[providerID+"|ext-1"] = t
	return t
}

func strPtr(s string) *string { return &s }

// --- TxManager --------------------------------------------------------------

func (f *fakeStore) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	return fn(ctx, &fakeRepos{store: f})
}

func (f *fakeStore) Repos() service.Repos { return &fakeRepos{store: f} }

// fakeRepos bundles the per-port adapters over one store.
type fakeRepos struct{ store *fakeStore }

func (r *fakeRepos) Wallets() service.WalletRepository { return fakeWallets{r.store} }
func (r *fakeRepos) Ledger() service.LedgerRepository  { return fakeLedger{r.store} }
func (r *fakeRepos) Transactions() service.TransactionRepository {
	return fakeTxs{r.store}
}
func (r *fakeRepos) Postings() service.PostingRepository { return fakePostings{r.store} }
func (r *fakeRepos) Outbox() service.OutboxRepository    { return fakeOutbox{r.store} }
func (r *fakeRepos) WalletOpens() service.WalletOpenRepository {
	return fakeWalletOpens{r.store}
}

// Inbox is never reached by the HTTP endpoints (they have no inbox); a nil
// interface makes an accidental dependency panic at the call site.
func (r *fakeRepos) Inbox() service.InboxRepository { return nil }

// --- WalletRepository -------------------------------------------------------

type fakeWallets struct{ store *fakeStore }

func (w fakeWallets) GetByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	if w.store.walletErr != nil {
		return nil, w.store.walletErr
	}
	if id != w.store.wallet.ID() {
		return nil, service.ErrRepoNotFound
	}
	return w.store.wallet, nil
}

func (w fakeWallets) Create(_ context.Context, wl *domain.Wallet) error {
	w.store.wallet = wl
	return nil
}
func (w fakeWallets) UpdateBalance(_ context.Context, wl *domain.Wallet) error {
	w.store.wallet = wl
	return nil
}
func (w fakeWallets) GetForUpdate(context.Context, uuid.UUID) (*domain.Wallet, error) {
	return w.store.wallet, nil
}
func (w fakeWallets) GetByPlayerAndCurrency(context.Context, uuid.UUID, string, bool) (*domain.Wallet, error) {
	return nil, service.ErrRepoNotFound
}
func (w fakeWallets) ListIDs(context.Context, *uuid.UUID, int) ([]uuid.UUID, error) { return nil, nil }

// --- LedgerRepository -------------------------------------------------------

type fakeLedger struct{ store *fakeStore }

func (l fakeLedger) Append(_ context.Context, e domain.LedgerEntry) error {
	entry := e
	l.store.entries = append(l.store.entries, &entry)
	return nil
}

func (l fakeLedger) ListByWallet(ctx context.Context, walletID uuid.UUID, cursor *service.LedgerCursor, limit int) ([]*domain.LedgerEntry, *service.LedgerCursor, error) {
	if l.store.ledgerErr != nil {
		return nil, nil, l.store.ledgerErr
	}
	return l.store.entries, nil, nil
}

func (l fakeLedger) Reconcile(ctx context.Context, walletID uuid.UUID) (stored, sum, count int64, err error) {
	if l.store.walletErr != nil {
		return 0, 0, 0, l.store.walletErr
	}
	return l.store.wallet.Balance().AmountMinor(), l.store.wallet.Balance().AmountMinor() + l.store.reconcileFix, 1, nil
}

// --- TransactionRepository --------------------------------------------------

type fakeTxs struct{ store *fakeStore }

func (t fakeTxs) GetByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	if t.store.txErr != nil {
		return nil, t.store.txErr
	}
	tx, ok := t.store.txs[id]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return tx, nil
}

func (t fakeTxs) GetByProviderAndExternal(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	if t.store.txErr != nil {
		return nil, t.store.txErr
	}
	tx, ok := t.store.txsByExt[providerID+"|"+externalID]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return tx, nil
}

func (t fakeTxs) Insert(_ context.Context, tx *domain.WagerTransaction) error {
	t.store.txs[tx.ID()] = tx
	t.store.txsByExt[tx.ProviderID()+"|"+tx.ExternalTxID()] = tx
	t.store.txsByKey[tx.ProviderID()+"|"+tx.IdempotencyKey()] = tx
	return nil
}

func (t fakeTxs) Save(_ context.Context, tx *domain.WagerTransaction) error {
	t.store.txs[tx.ID()] = tx
	return nil
}

func (t fakeTxs) GetByProviderAndKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	if t.store.txErr != nil {
		return nil, t.store.txErr
	}
	tx, ok := t.store.txsByKey[providerID+"|"+key]
	if !ok {
		return nil, service.ErrRepoNotFound
	}
	return tx, nil
}
func (t fakeTxs) ExistsProcessedReversal(context.Context, uuid.UUID, domain.TxKind) (bool, error) {
	return false, nil
}
func (t fakeTxs) ClaimPendingReferences(context.Context, time.Time, int) ([]*domain.WagerTransaction, error) {
	return nil, nil
}

// --- Instrumentation: a no-op hook ------------------------------------------

func (f *fakeStore) WagerOutcome(string, string, string, string) {}
func (f *fakeStore) Duplicate(string)                            {}
func (f *fakeStore) ReferenceOutcome(string)                     {}
func (f *fakeStore) ReconciliationDivergence()                   {}
func (f *fakeStore) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttributes(...service.SpanAttr) {}
func (noopSpan) SetOutcome(string, string)         {}
func (noopSpan) RecordError(error)                 {}
func (noopSpan) End()                              {}

var _ service.Instrumentation = (*fakeStore)(nil)

// --- PostingRepository / OutboxRepository / WalletOpenRepository -------------

type fakePostings struct{ store *fakeStore }

func (p fakePostings) Append(_ context.Context, e domain.JournalEntry) error {
	p.store.postings = append(p.store.postings, e)
	return nil
}

type fakeOutbox struct{ store *fakeStore }

func (o fakeOutbox) Enqueue(_ context.Context, envs []domain.Envelope) error {
	o.store.outbox = append(o.store.outbox, envs...)
	return nil
}
func (o fakeOutbox) ClaimBatch(context.Context, time.Time, time.Duration, int) ([]service.OutboxItem, error) {
	return nil, nil
}
func (o fakeOutbox) MarkPublished(context.Context, uuid.UUID, time.Time) error { return nil }
func (o fakeOutbox) MarkFailed(context.Context, uuid.UUID, time.Time, string) error {
	return nil
}
func (o fakeOutbox) Stats(context.Context) (int64, float64, error) { return 0, 0, nil }

// fakeWalletOpens implements the opening idempotency ledger: a key maps to the
// wallet it created. The default (no key recorded) is "first time", which is
// what the open-wallet endpoint test wants.
type fakeWalletOpens struct{ store *fakeStore }

func (w fakeWalletOpens) Record(_ context.Context, key, _ string, walletID uuid.UUID) error {
	w.store.openKeys[key] = walletID
	return nil
}
func (w fakeWalletOpens) Lookup(_ context.Context, key string) (uuid.UUID, string, error) {
	id, ok := w.store.openKeys[key]
	if !ok {
		return uuid.Nil, "", service.ErrRepoNotFound
	}
	return id, "", nil
}
