package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// The double-entry layer's whole claim is that every wallet movement gets a
// counterparty leg, written in the same SQL transaction, and that the resulting
// journal balances. These tests assert that claim at the service boundary — the
// place a caller actually reaches — rather than only on domain.NewJournalEntry.

// soleJournal returns the one journal entry the store recorded, failing if the
// count differs.
func soleJournal(t *testing.T, store *fakeStore) domain.JournalEntry {
	t.Helper()
	require.Len(t, store.journal, 1, "exactly one journal entry")
	return store.journal[0]
}

// postingFor finds the leg on a given account key within an entry.
func postingFor(t *testing.T, e domain.JournalEntry, key string) domain.Posting {
	t.Helper()
	for _, p := range e.Postings() {
		if p.Account().Key() == key {
			return p
		}
	}
	require.Failf(t, "posting missing", "no leg for account %s in %v", key, e.Postings())
	return domain.Posting{}
}

func TestBetWritesBalancedJournalEntry(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	res, err := newWagerService(store).Submit(context.Background(), betInput(w))
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, res.Transaction.Status())

	entry := soleJournal(t, store)
	assert.Equal(t, res.Transaction.ID(), entry.TransactionID())
	assert.True(t, entry.Balanced(), "debits equal credits")

	// A BET debits the player and credits the house, for the same amount.
	player := postingFor(t, entry, domain.PlayerWalletAccount(w.ID()).Key())
	assert.Equal(t, domain.DirectionDebit, player.Direction())
	assert.Equal(t, "10.00", player.Amount().String())

	house := postingFor(t, entry, domain.HouseAccount().Key())
	assert.Equal(t, domain.DirectionCredit, house.Direction())
	assert.Equal(t, "10.00", house.Amount().String())

	// The player leg must equal the ledger row: one movement, two views of it.
	require.Len(t, store.ledger, 1)
	assert.Equal(t, store.ledger[0].Direction(), player.Direction())
	assert.Equal(t, store.ledger[0].Amount().String(), player.Amount().String())

	// The domain's own delta helper agrees with the wallet it moved.
	delta, err := entry.PlayerDelta(w.ID())
	require.NoError(t, err)
	assert.Equal(t, "-10.00", delta.String())
	assert.Equal(t, "90.00", store.wallet(t).Balance().String())
}

func TestWinWritesBalancedJournalEntry(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	in := betInput(w)
	in.Kind = "WIN"
	res, err := newWagerService(store).Submit(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, res.Transaction.Status())

	entry := soleJournal(t, store)
	assert.True(t, entry.Balanced())

	// A WIN is the mirror image: house debits, player credits.
	player := postingFor(t, entry, domain.PlayerWalletAccount(w.ID()).Key())
	assert.Equal(t, domain.DirectionCredit, player.Direction())
	house := postingFor(t, entry, domain.HouseAccount().Key())
	assert.Equal(t, domain.DirectionDebit, house.Direction())

	delta, err := entry.PlayerDelta(w.ID())
	require.NoError(t, err)
	assert.Equal(t, "10.00", delta.String())
	assert.Equal(t, "110.00", store.wallet(t).Balance().String())
}

func TestOpeningWritesJournalFromTheFirstRow(t *testing.T) {
	// The fake store births the wallet directly (no OPENING transaction), so
	// exercise the real opening path through WalletService. A faultStore's
	// delegate is a full working store, so its WalletService opens for real.
	store := newFaultStore(t, "0.00")
	svc := store.walletService()

	_, err := svc.Open(context.Background(), service.OpenWalletInput{
		PlayerID:       domain.MustNewID().String(),
		Currency:       "BRL",
		InitialAmount:  "50.00",
		IdempotencyKey: "open-" + domain.MustNewID().String(),
		CorrelationID:  "corr-open",
	})
	require.NoError(t, err)

	entry := soleJournal(t, store.fakeStore)
	assert.True(t, entry.Balanced(), "the opening credit has a debiting counterparty")

	// Exactly one credit (the player) and one debit (the house), same amount.
	assert.Equal(t, "50.00", entry.TotalCredits().String())
	assert.Equal(t, "50.00", entry.TotalDebits().String())

	// The player leg names the wallet that was just opened; find that leg by
	// kind rather than by a key we would have to guess (the store already held
	// a wallet of its own before Open ran).
	var player domain.Posting
	playerFound := false
	for _, p := range entry.Postings() {
		if p.Account().Kind() == domain.AccountPlayerWallet {
			player, playerFound = p, true
		}
	}
	require.True(t, playerFound, "the entry has a player leg")
	assert.Equal(t, domain.DirectionCredit, player.Direction(), "a wallet opening credits the player")
	assert.Equal(t, "50.00", player.Amount().String())

	// The opening's journal agrees with the ledger row written beside it.
	require.Len(t, store.ledger, 1, "the opening wrote its ledger row")
	assert.Equal(t, store.ledger[0].Direction(), player.Direction())
	assert.Equal(t, store.ledger[0].Amount().String(), player.Amount().String())
	assert.Equal(t, store.ledger[0].WalletID().String(), player.Account().Ref(), "the player leg names the ledger's wallet")
}

func TestLossWritesNoJournal(t *testing.T) {
	store := newFakeStore(t, "100.00")
	w := store.wallet(t)

	in := betInput(w)
	in.Kind = "LOSS"
	in.Amount = "0.00"
	res, err := newWagerService(store).Submit(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, domain.StatusProcessed, res.Transaction.Status())

	assert.Empty(t, store.journal, "a fee-less LOSS moves no money, so it posts nothing")
	assert.Empty(t, store.ledger, "and writes no ledger row either")
}

// The rollback half of the claim — that a journal failure takes the money down
// with it — is NOT asserted here on purpose. The fake TxManager runs the
// callback inline with no rollback semantics (see its WithinTx), so a unit
// assertion would pass for the wrong reason or, worse, fail while production is
// fine. The property belongs where a real transaction exists:
// TestJournalFailureRollsBackMovement in test/integration, against Postgres,
// where WithinTx is pgx.BeginFunc and a returned error really does discard the
// ledger row, the journal entry and the balance update together.
