package domain_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// Unit tests for the double-entry layer: the chart of accounts, postings, and
// the invariant that every journal entry balances (total debits == total
// credits). These are pure-domain tests — no database, no transport.

func TestAccountKindsAreClosed(t *testing.T) {
	for _, k := range []domain.AccountKind{
		domain.AccountPlayerWallet, domain.AccountHouse,
		domain.AccountProviderSettlement, domain.AccountRevenue,
	} {
		assert.True(t, k.Valid(), "%s must be a known kind", k)
		returned := domain.AccountKind(k.String())
		assert.Equal(t, k, returned)
	}
	for _, k := range []domain.AccountKind{"", "PLAYER", "player_wallet", "CASH"} {
		assert.False(t, k.Valid(), "%q must be rejected", k)
	}
}

func TestNewAccountReferenceRules(t *testing.T) {
	walletID := uuid.NewString()

	tests := []struct {
		name    string
		kind    domain.AccountKind
		ref     string
		wantErr bool
		why     string
	}{
		{"player wallet with uuid ref", domain.AccountPlayerWallet, walletID, false, "the happy path"},
		{"player wallet without ref", domain.AccountPlayerWallet, "", true, "a wallet account is meaningless without its wallet"},
		{"player wallet with non-uuid ref", domain.AccountPlayerWallet, "not-a-uuid", true, "the reference must be a wallet identity"},
		{"house singleton", domain.AccountHouse, "", false, "house is a singleton"},
		{"house with ref", domain.AccountHouse, "x", true, "a singleton must not be parameterised"},
		{"revenue singleton", domain.AccountRevenue, "", false, "revenue is a singleton"},
		{"revenue with ref", domain.AccountRevenue, "x", true, "a singleton must not be parameterised"},
		{"provider settlement with ref", domain.AccountProviderSettlement, "provider-a", false, "settlement is per provider"},
		{"provider settlement without ref", domain.AccountProviderSettlement, "", true, "which provider would it settle?"},
		{"unknown kind", domain.AccountKind("CASH"), "x", true, "unknown kinds fail closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc, err := domain.NewAccount(tc.kind, tc.ref)
			if tc.wantErr {
				require.Error(t, err, tc.why)
				assert.ErrorIs(t, err, domain.ErrInvalidAccount, "must be a classified domain error")
				assert.Zero(t, acc)
				return
			}
			require.NoError(t, err, tc.why)
			assert.Equal(t, tc.kind, acc.Kind())
			assert.Equal(t, tc.ref, acc.Ref())
		})
	}
}

func TestAccountKeyIsStableAndSortable(t *testing.T) {
	walletID := uuid.New()
	acc := domain.PlayerWalletAccount(walletID)
	assert.Equal(t, "PLAYER_WALLET:"+walletID.String(), acc.Key())
	assert.Equal(t, acc.Key(), acc.String())

	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)
	assert.Equal(t, "HOUSE:", house.Key(), "a singleton keeps a trailing separator so keys never collide")
	assert.NotEqual(t, acc.Key(), house.Key())
}

func TestPlayerWalletAccountConvenience(t *testing.T) {
	id := uuid.New()
	acc := domain.PlayerWalletAccount(id)
	assert.Equal(t, domain.AccountPlayerWallet, acc.Kind())
	assert.Equal(t, id.String(), acc.Ref())

	// It must equal the explicitly constructed account (same identity).
	explicit, err := domain.NewAccount(domain.AccountPlayerWallet, id.String())
	require.NoError(t, err)
	assert.Equal(t, explicit.Key(), acc.Key())
}

func TestNewPostingRejectsNonMovementsAndZero(t *testing.T) {
	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)

	ten, err := domain.NewMoney(1000, "BRL")
	require.NoError(t, err)
	zero, err := domain.NewMoney(0, "BRL")
	require.NoError(t, err)

	tests := []struct {
		name    string
		dir     domain.Direction
		amount  domain.Money
		wantErr bool
	}{
		{"debit", domain.DirectionDebit, ten, false},
		{"credit", domain.DirectionCredit, ten, false},
		{"none direction", domain.DirectionNone, ten, true},
		{"empty direction", domain.Direction(""), ten, true},
		{"zero amount", domain.DirectionDebit, zero, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := domain.NewPosting(house, tc.dir, tc.amount)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.dir, p.Direction())
			assert.Equal(t, tc.amount.AmountMinor(), p.Amount().AmountMinor())
		})
	}
}

func TestPostingSignedFollowsAccountingConvention(t *testing.T) {
	house, _ := domain.NewAccount(domain.AccountHouse, "")
	ten, _ := domain.NewMoney(1000, "BRL")

	debit, err := domain.NewPosting(house, domain.DirectionDebit, ten)
	require.NoError(t, err)
	assert.Equal(t, int64(-1000), debit.Signed().AmountMinor(), "a debit is negative in the balancing sum")

	credit, err := domain.NewPosting(house, domain.DirectionCredit, ten)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), credit.Signed().AmountMinor(), "a credit is positive in the balancing sum")
}

func TestNewJournalEntryRejectsMalformed(t *testing.T) {
	house, _ := domain.NewAccount(domain.AccountHouse, "")
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	now := time.Now().UTC()

	ten, _ := domain.NewMoney(1000, "BRL")
	twenty, _ := domain.NewMoney(2000, "BRL")
	tenUSD, _ := domain.NewMoney(1000, "USD")

	dPlayer, _ := domain.NewPosting(player, domain.DirectionDebit, ten)
	cHouse, _ := domain.NewPosting(house, domain.DirectionCredit, ten)
	dHouse, _ := domain.NewPosting(house, domain.DirectionDebit, ten)
	cPlayer, _ := domain.NewPosting(player, domain.DirectionCredit, ten)
	dHouse20, _ := domain.NewPosting(house, domain.DirectionDebit, twenty)
	dPlayerUSD, _ := domain.NewPosting(player, domain.DirectionDebit, tenUSD)

	tests := []struct {
		name     string
		txID     uuid.UUID
		currency string
		postings []domain.Posting
		wantErr  error
		why      string
	}{
		{"nil transaction id", uuid.Nil, "BRL", []domain.Posting{dPlayer, cHouse},
			domain.ErrLedgerArithmetic, "an entry must belong to a transaction"},
		{"no postings", uuid.New(), "BRL", nil,
			domain.ErrUnbalancedJournal, "an entry that moves nothing is not a journal entry"},
		{"empty postings slice", uuid.New(), "BRL", []domain.Posting{},
			domain.ErrUnbalancedJournal, "same as no postings"},
		{"debits do not equal credits", uuid.New(), "BRL", []domain.Posting{dHouse20, cHouse},
			domain.ErrUnbalancedJournal, "2000 debit vs 1000 credit does not balance"},
		{"two debits one credit", uuid.New(), "BRL", []domain.Posting{dPlayer, dHouse, cPlayer},
			domain.ErrUnbalancedJournal, "2000 debit vs 1000 credit does not balance"},
		{"cross-currency posting", uuid.New(), "BRL", []domain.Posting{dPlayerUSD, cHouse},
			domain.ErrCurrencyMismatch, "a journal entry is single-currency"},
		{"balanced", uuid.New(), "BRL", []domain.Posting{dPlayer, cHouse},
			nil, "the happy path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := domain.NewJournalEntry(tc.txID, tc.currency, tc.postings, now)
			if tc.wantErr != nil {
				require.Error(t, err, tc.why)
				assert.ErrorIs(t, err, tc.wantErr, tc.why)
				assert.Zero(t, entry.TransactionID())
				return
			}
			require.NoError(t, err, tc.why)
			assert.Equal(t, tc.txID, entry.TransactionID())
			assert.True(t, entry.Balanced())
		})
	}
}

func TestJournalEntryPostingsAreSortedDeterministically(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, _ := domain.NewAccount(domain.AccountHouse, "")
	now := time.Now().UTC()
	ten, _ := domain.NewMoney(1000, "BRL")

	dPlayer, _ := domain.NewPosting(player, domain.DirectionDebit, ten)
	cHouse, _ := domain.NewPosting(house, domain.DirectionCredit, ten)

	// Feed the postings in the "wrong" order and assert the stored order.
	entry, err := domain.NewJournalEntry(uuid.New(), "BRL", []domain.Posting{cHouse, dPlayer}, now)
	require.NoError(t, err)

	got := entry.Postings()
	require.Len(t, got, 2)
	assert.Equal(t, house.Key(), got[0].Account().Key(), "HOUSE sorts before PLAYER_WALLET")
	assert.Equal(t, player.Key(), got[1].Account().Key())

	// The same postings in either input order produce the same representation.
	again, err := domain.NewJournalEntry(entry.TransactionID(), "BRL", []domain.Posting{dPlayer, cHouse}, now)
	require.NoError(t, err)
	assert.Equal(t, entry.Postings(), again.Postings(), "ordering must be canonical")

	// Postings() returns a copy: mutating it must not corrupt the entry.
	got[0] = dPlayer
	assert.Equal(t, house.Key(), entry.Postings()[0].Account().Key(), "no aliasing of internal slice")
}

func TestJournalTotalsAndBalance(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, _ := domain.NewAccount(domain.AccountHouse, "")
	revenue, _ := domain.NewAccount(domain.AccountRevenue, "")
	now := time.Now().UTC()

	ten, _ := domain.NewMoney(1000, "BRL")
	three, _ := domain.NewMoney(300, "BRL")
	seven, _ := domain.NewMoney(700, "BRL")

	dPlayer, _ := domain.NewPosting(player, domain.DirectionDebit, ten)
	cRevenue, _ := domain.NewPosting(revenue, domain.DirectionCredit, three)
	cHouse, _ := domain.NewPosting(house, domain.DirectionCredit, seven)

	entry, err := domain.NewJournalEntry(uuid.New(), "BRL", []domain.Posting{dPlayer, cRevenue, cHouse}, now)
	require.NoError(t, err, "1000 debit must equal 300+700 credit")

	assert.Equal(t, int64(1000), entry.TotalDebits().AmountMinor())
	assert.Equal(t, int64(1000), entry.TotalCredits().AmountMinor())
	assert.True(t, entry.Balanced())
	assert.Equal(t, "BRL", entry.Currency())
	assert.Equal(t, now, entry.CreatedAt())
}

func TestJournalPlayerDelta(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, _ := domain.NewAccount(domain.AccountHouse, "")
	now := time.Now().UTC()
	twentyFive, _ := domain.NewMoney(2500, "BRL")

	playerDebit, _ := domain.NewPosting(player, domain.DirectionDebit, twentyFive)
	houseCredit, _ := domain.NewPosting(house, domain.DirectionCredit, twentyFive)

	// A BET debits the player: the delta on the player account is negative.
	bet, err := domain.NewJournalEntry(uuid.New(), "BRL", []domain.Posting{playerDebit, houseCredit}, now)
	require.NoError(t, err)
	delta, err := bet.PlayerDelta(walletID)
	require.NoError(t, err)
	assert.Equal(t, int64(-2500), delta.AmountMinor(), "a debit reduces the player's holding")

	// A WIN credits the player: the mirrored entry has a positive delta.
	playerCredit, _ := domain.NewPosting(player, domain.DirectionCredit, twentyFive)
	houseDebit, _ := domain.NewPosting(house, domain.DirectionDebit, twentyFive)
	win, err := domain.NewJournalEntry(uuid.New(), "BRL", []domain.Posting{playerCredit, houseDebit}, now)
	require.NoError(t, err)
	delta, err = win.PlayerDelta(walletID)
	require.NoError(t, err)
	assert.Equal(t, int64(2500), delta.AmountMinor(), "a credit increases the player's holding")

	// An entry for another wallet has no postings for this one: an explicit error,
	// never a silent zero.
	otherWallet := uuid.New()
	_, err = bet.PlayerDelta(otherWallet)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidAccount)
}

// TestJournalOneSidedOverflowFailsAtConstruction pins WHY the Money.Add error
// branch inside PlayerDelta is unreachable, rather than pretending to exercise
// it.
//
// PlayerDelta sums the SIGNED legs of one account, so in principle an entry
// could balance (each side representable) while a partial signed sum leaves
// int64. That entry cannot be built, and this test proves the reason is
// structural, not incidental: NewJournalEntry checks Balanced() during
// construction, Balanced() calls total(DirectionDebit) first, and total() panics
// on a one-sided overflow. So the very entry that would give PlayerDelta an
// overflowing partial is refused — loudly — before an entry exists to ask.
//
// The consequence is deliberate: PlayerDelta's error return is defensive only.
// Every reachable caller sees either a delta or ErrInvalidAccount. We assert the
// reachable half (a zero-value entry has no postings -> fail closed) and the
// unreachable half's guard (construction panics rather than handing back a
// half-summed entry).
func TestJournalOneSidedOverflowFailsAtConstruction(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)

	// A zero-value entry never reached NewJournalEntry: it has no postings, so
	// PlayerDelta fails closed instead of inventing a zero delta. This is the
	// only error PlayerDelta can return in practice.
	var zero domain.JournalEntry
	_, err = zero.PlayerDelta(walletID)
	require.Error(t, err, "an entry with no postings must not report a delta")
	assert.ErrorIs(t, err, domain.ErrInvalidAccount)

	max, err := domain.NewMoney(math.MaxInt64, "BRL")
	require.NoError(t, err)
	// -MaxInt64 is the deepest debit Money can produce: Neg refuses MinInt64, so
	// Neg.(-MaxInt64) alone fits. Two of them on one side do not.
	negMax, err := max.Neg()
	require.NoError(t, err)
	assert.Equal(t, int64(-math.MaxInt64), negMax.AmountMinor())

	cPlayer, err := domain.NewPosting(player, domain.DirectionCredit, max)
	require.NoError(t, err)
	cPlayer2, err := domain.NewPosting(player, domain.DirectionCredit, max)
	require.NoError(t, err)
	dPlayer, err := domain.NewPosting(player, domain.DirectionDebit, max)
	require.NoError(t, err)
	cHouse, err := domain.NewPosting(house, domain.DirectionCredit, max)
	require.NoError(t, err)
	dHouse, err := domain.NewPosting(house, domain.DirectionDebit, max)
	require.NoError(t, err)
	dHouse2, err := domain.NewPosting(house, domain.DirectionDebit, max)
	require.NoError(t, err)

	// Two max credits on the player side overflow total(CREDIT) — and therefore
	// panic during Balanced(), during construction. The entry that would have
	// made PlayerDelta overflow can never come into being.
	txID := uuid.New()
	assert.PanicsWithValue(t,
		fmt.Sprintf("journal total overflow for transaction %s: %v", txID, domain.ErrMoneyOverflow),
		func() {
			_, _ = domain.NewJournalEntry(txID, "BRL",
				[]domain.Posting{cPlayer, cPlayer2, dPlayer, cHouse, dHouse, dHouse2}, time.Now().UTC())
		})
}

// TestJournalTotalsFailLoudOnOverflow pins the deliberate panic in total().
//
// An entry whose two sides carry DIFFERENT totals balances to false, but
// NewJournalEntry rejects that before the entry exists — so the only way to
// reach total() with a side that overflows while construction still succeeds is
// for Balanced() to ask the overflowing question. It does: it computes
// TotalDebits() first, so two max-valued debit legs against one max credit leg
// panic during construction, at the summing, and not at the comparison.
//
// That ordering is the point of this test. A silent partial sum would have made
// Balanced() compare garbage instead of failing, and Balanced() is what decides
// whether an entry is written to the journal at all.
func TestJournalTotalsFailLoudOnOverflow(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)
	now := time.Now().UTC()

	max, err := domain.NewMoney(math.MaxInt64, "BRL")
	require.NoError(t, err)
	// Two max debit legs (their sum overflows) against one max credit leg.
	dHouse, err := domain.NewPosting(house, domain.DirectionDebit, max)
	require.NoError(t, err)
	cHouse, err := domain.NewPosting(house, domain.DirectionCredit, max)
	require.NoError(t, err)
	cPlayer, err := domain.NewPosting(player, domain.DirectionCredit, max)
	require.NoError(t, err)

	txID := uuid.New()
	assert.PanicsWithValue(t,
		fmt.Sprintf("journal total overflow for transaction %s: %v", txID, domain.ErrMoneyOverflow),
		func() {
			_, _ = domain.NewJournalEntry(txID, "BRL", []domain.Posting{dHouse, cHouse, cPlayer}, now)
		})
}

func TestEntryForMovementDirectionMapping(t *testing.T) {
	walletID := uuid.New()
	txID := uuid.New()
	now := time.Now().UTC()
	twentyFive, _ := domain.NewMoney(2500, "BRL")

	tests := []struct {
		name      string
		kind      domain.TxKind
		movement  domain.Movement
		wantEntry bool
		wantDelta int64
		why       string
	}{
		{
			name: "BET debits the player and credits the house",
			kind: domain.KindBet,
			movement: domain.Movement{HasChange: true, Direction: domain.DirectionDebit,
				Amount: twentyFive, BalanceBefore: twentyFive, BalanceAfter: twentyFive},
			wantEntry: true, wantDelta: -2500,
			why: "a stake leaves the player and enters the house",
		},
		{
			name: "WIN credits the player and debits the house",
			kind: domain.KindWin,
			movement: domain.Movement{HasChange: true, Direction: domain.DirectionCredit,
				Amount: twentyFive, BalanceBefore: twentyFive, BalanceAfter: twentyFive},
			wantEntry: true, wantDelta: 2500,
			why: "the house pays the win",
		},
		{
			name: "REFUND credits the player (reversal of a BET)",
			kind: domain.KindRefund,
			movement: domain.Movement{HasChange: true, Direction: domain.DirectionCredit,
				Amount: twentyFive, BalanceBefore: twentyFive, BalanceAfter: twentyFive},
			wantEntry: true, wantDelta: 2500,
			why: "a refund returns the stake to the player",
		},
		{
			name: "ROLLBACK credits the player",
			kind: domain.KindRollback,
			movement: domain.Movement{HasChange: true, Direction: domain.DirectionCredit,
				Amount: twentyFive, BalanceBefore: twentyFive, BalanceAfter: twentyFive},
			wantEntry: true, wantDelta: 2500,
			why: "a rollback undoes a debit",
		},
		{
			name:      "no movement (LOSS carries zero) writes no entry",
			kind:      domain.KindLoss,
			movement:  domain.Movement{HasChange: false, Direction: domain.DirectionNone},
			wantEntry: false,
			why:       "a LOSS is informational: it must not appear in the journal",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok, err := domain.EntryForMovement(walletID, txID, tc.kind, tc.movement, now)
			require.NoError(t, err, tc.why)
			if !tc.wantEntry {
				assert.False(t, ok, tc.why)
				assert.Zero(t, entry.TransactionID(), "no entry must be returned")
				return
			}
			require.True(t, ok, tc.why)
			assert.True(t, entry.Balanced(), "every synthesised entry must balance")
			assert.Equal(t, txID, entry.TransactionID())

			delta, err := entry.PlayerDelta(walletID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDelta, delta.AmountMinor(), tc.why)

			// Exactly the two value legs: player and house.
			postings := entry.Postings()
			require.Len(t, postings, 2, "a movement has exactly one counterparty leg")
			assert.Equal(t, int64(2500), entry.TotalDebits().AmountMinor())
			assert.Equal(t, int64(2500), entry.TotalCredits().AmountMinor())
		})
	}
}

func TestEntryForMovementRejectsUnreconstructableMovement(t *testing.T) {
	walletID := uuid.New()
	txID := uuid.New()
	zero, _ := domain.NewMoney(0, "BRL")

	// HasChange true with a non-positive amount cannot be produced by the wallet,
	// but the journal must fail closed rather than emit an unbalanced entry.
	_, _, err := domain.EntryForMovement(walletID, txID, domain.KindBet, domain.Movement{
		HasChange: true, Direction: domain.DirectionDebit, Amount: zero,
	}, time.Now().UTC())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)

	// An unknown direction with a real amount is equally rejected.
	twentyFive, _ := domain.NewMoney(2500, "BRL")
	_, _, err = domain.EntryForMovement(walletID, txID, domain.KindBet, domain.Movement{
		HasChange: true, Direction: domain.Direction("SIDEWAYS"), Amount: twentyFive,
	}, time.Now().UTC())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
}

// TestPlayerDeltaOrderIsIndependentOfPostingInputOrder is the property the
// overflow walk depends on: PlayerDelta accumulates over the postings in the
// order the journal stored them, so the result is only well defined if that
// order does not depend on the order the caller happened to pass the legs in.
//
// The stakes are concrete. The legs of one account are indistinguishable by the
// sort key, and NewJournalEntry uses sort.Slice, which is NOT stable — for
// slices above sort's insertion-sort threshold equal keys may come out in any
// order. Every entry the service currently builds has two legs, so it is
// exercised only below that threshold, where the order does happen to be
// preserved. This test pins the property rather than the mechanism: same legs
// in, same delta out, whatever the length — the exact check that would fail the
// day an unbalanced-but-overflowing entry like the one above became reachable.
func TestPlayerDeltaOrderIsIndependentOfPostingInputOrder(t *testing.T) {
	walletID := uuid.New()
	player := domain.PlayerWalletAccount(walletID)
	house, err := domain.NewAccount(domain.AccountHouse, "")
	require.NoError(t, err)
	now := time.Now().UTC()

	ten, err := domain.NewMoney(1000, "BRL")
	require.NoError(t, err)
	three, err := domain.NewMoney(300, "BRL")
	require.NoError(t, err)

	cPlayer, err := domain.NewPosting(player, domain.DirectionCredit, ten)
	require.NoError(t, err)
	dPlayer, err := domain.NewPosting(player, domain.DirectionDebit, three)
	require.NoError(t, err)
	// Mirrored counterparty legs: the same two amounts, opposite directions,
	// which is what keeps all four arrangements balanced.
	dHouse, err := domain.NewPosting(house, domain.DirectionDebit, ten)
	require.NoError(t, err)
	cHouse, err := domain.NewPosting(house, domain.DirectionCredit, three)
	require.NoError(t, err)

	// player: -300 +1000 = +700 regardless of the order the legs arrive in.
	legs := []domain.Posting{cPlayer, dPlayer, dHouse, cHouse}
	arrangements := [][]domain.Posting{
		{cPlayer, dPlayer, dHouse, cHouse},
		{dPlayer, cPlayer, cHouse, dHouse},
		{dHouse, cHouse, cPlayer, dPlayer},
		{cHouse, dHouse, dPlayer, cPlayer},
	}
	for i, in := range arrangements {
		entry, err := domain.NewJournalEntry(uuid.New(), "BRL", in, now)
		require.NoError(t, err, "arrangement %d must balance", i)
		require.Len(t, entry.Postings(), len(legs))

		// Stability means the legs of the same account keep the CALLER's relative
		// order — not a hardcoded one. Pull the player's legs out of the input in
		// order and require the journal to have preserved exactly that.
		var inputDirs, storedDirs []domain.Direction
		for _, p := range in {
			if p.Account().Key() == player.Key() {
				inputDirs = append(inputDirs, p.Direction())
			}
		}
		for _, p := range entry.Postings() {
			if p.Account().Key() == player.Key() {
				storedDirs = append(storedDirs, p.Direction())
			}
		}
		assert.Equal(t, inputDirs, storedDirs,
			"arrangement %d: equal-key legs must keep their input order (sort must be stable)", i)

		// And the property that actually matters: the result must not depend on
		// how the caller ordered the legs.
		delta, err := entry.PlayerDelta(walletID)
		require.NoError(t, err)
		assert.Equal(t, int64(700), delta.AmountMinor(),
			"arrangement %d: the delta must not depend on the caller's leg order", i)
	}
}

// arms of EntryForMovement: the function synthesises two postings and a journal
// entry, and a failure in either must reach the caller unchanged rather than be
// swallowed or replaced by a generic error.
//
// The nil transaction id is the reachable trigger: NewPosting accepts the legs
// (a zero uuid is still a valid account reference), and NewJournalEntry is the
// one that refuses it. So the error must be ErrLedgerArithmetic and nothing may
// be returned alongside it — a half-built entry must never escape.
func TestEntryForMovementPropagatesPostingAndJournalErrors(t *testing.T) {
	walletID := uuid.New()
	now := time.Now().UTC()
	stake, err := domain.NewMoney(2500, "BRL")
	require.NoError(t, err)
	movement := domain.Movement{
		HasChange: true, Direction: domain.DirectionDebit, Amount: stake,
	}

	entry, ok, err := domain.EntryForMovement(walletID, uuid.Nil, domain.KindBet, movement, now)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	assert.False(t, ok, "ok must be false whenever an error is returned")
	assert.Equal(t, uuid.Nil, entry.TransactionID(), "no partial entry may leak out")

	// The same movement with a real transaction id does build the entry, which
	// is what makes the assertion above about the id and not about the movement.
	entry, ok, err = domain.EntryForMovement(walletID, uuid.New(), domain.KindBet, movement, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, entry.Postings(), 2)
}

// TestEntryForMovementRejectsUnknownDirectionAtPosting is the narrower claim
// that the posting arms are exercised independently of the journal gate: a
// movement that is neither DEBIT nor CREDIT and carries a positive amount gets
// past the HasChange/positiveness guards and is stopped by the direction switch.
func TestEntryForMovementRejectsUnknownDirectionAtPosting(t *testing.T) {
	walletID := uuid.New()
	stake, err := domain.NewMoney(2500, "BRL")
	require.NoError(t, err)

	for _, dir := range []domain.Direction{"", "SIDEWAYS", "Hold"} {
		_, ok, err := domain.EntryForMovement(walletID, uuid.New(), domain.KindBet, domain.Movement{
			HasChange: true, Direction: dir, Amount: stake,
		}, time.Now().UTC())
		require.Error(t, err, "direction %q must be refused", dir)
		assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
		assert.False(t, ok)
	}
}

// TestDoubleEntryMirrorsWalletLedger is the property that ties the new layer to
// the existing one: for a bet and its refund, the player-facing delta of the
// journal equals the signed balance movement the wallet reports, and the two
// entries net to zero. This is the accounting identity the whole feature exists
// to make checkable.
func TestDoubleEntryMirrorsWalletLedger(t *testing.T) {
	wallet, _, err := domain.OpenWallet(uuid.New(), "BRL", domain.ZeroMoneyMust("BRL"), time.Now().UTC())
	require.NoError(t, err)
	// Fund the wallet so the bet below has a balance to draw from.
	seed, err := domain.NewMoney(5000, "BRL")
	require.NoError(t, err)
	_, err = wallet.Credit(seed, time.Now().UTC())
	require.NoError(t, err)
	now := time.Now().UTC()

	stake, err := domain.NewMoney(2500, "BRL")
	require.NoError(t, err)

	// BET: debit the wallet, then build the journal entry from the movement.
	betMv, err := wallet.Debit(stake, now)
	require.NoError(t, err)
	betEntry, ok, err := domain.EntryForMovement(wallet.ID(), uuid.New(), domain.KindBet, betMv, now)
	require.NoError(t, err)
	require.True(t, ok)

	betDelta, err := betEntry.PlayerDelta(wallet.ID())
	require.NoError(t, err)
	ledgerDelta := betMv.BalanceAfter.AmountMinor() - betMv.BalanceBefore.AmountMinor()
	assert.Equal(t, ledgerDelta, betDelta.AmountMinor(),
		"the journal's player delta must equal the wallet's balance movement")

	// REFUND: credit the wallet back, build the mirrored entry.
	refundMv, err := wallet.Credit(stake, now)
	require.NoError(t, err)
	refundEntry, ok, err := domain.EntryForMovement(wallet.ID(), uuid.New(), domain.KindRefund, refundMv, now)
	require.NoError(t, err)
	require.True(t, ok)

	refundDelta, err := refundEntry.PlayerDelta(wallet.ID())
	require.NoError(t, err)
	refundLedgerDelta := refundMv.BalanceAfter.AmountMinor() - refundMv.BalanceBefore.AmountMinor()
	assert.Equal(t, refundLedgerDelta, refundDelta.AmountMinor())

	// Bet and refund cancel out: the two entries net to zero on the player side.
	net, err := betDelta.Add(refundDelta)
	require.NoError(t, err)
	assert.True(t, net.IsZero(), "a bet followed by a full refund must net to zero")
}
