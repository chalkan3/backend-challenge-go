package domain

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Double-entry accounting on top of the existing single-sided ledger.
//
// The wallet already enforces, per movement, that
// `balance_after = balance_before ± amount` and that the balance equals the sum
// of the ledger (checked by loadtest/sql/invariants.sql audit #4). What it does
// NOT express is the other leg: every debit is also a credit somewhere, so the
// system as a whole conserves value. This file adds that second leg.
//
// Model
//
//	Account        — a named bucket of value (a player's wallet, the house, a
//	                 provider's settlement account). Identified by (kind, ref).
//	Posting        — one immutable leg: move `amount` in `direction` on one
//	                 account.
//	JournalEntry   — the set of postings for one transaction. The invariant is
//	                 that it BALANCES: total debits == total credits, in one
//	                 currency. Zero is allowed and carries no postings (LOSS).
//
// The double-entry invariant is therefore a property of the journal, not of a
// single account, and it is checkable with one aggregate query:
//
//	SELECT transaction_id FROM postings
//	GROUP BY transaction_id
//	HAVING SUM(CASE WHEN direction='DEBIT' THEN amount_minor ELSE -amount_minor END) <> 0;
//
// The player-facing side (AccountPlayerWallet) deliberately mirrors the wallet
// row, so the existing ledger, reconciliation and balance columns keep working
// unchanged: double entry is added as an auditing layer, not as a replacement
// for the balance.

// AccountKind classifies an account in the chart of accounts. The numeric
// sign convention is documented per kind; it is what makes the journal balance.
type AccountKind string

const (
	// AccountPlayerWallet is a player's wallet: credits increase their held
	// value (the provider owes the player), debits decrease it.
	AccountPlayerWallet AccountKind = "PLAYER_WALLET"
	// AccountHouse is the operator's house account — the counterparty of every
	// player movement. A player debit is a house credit and vice versa.
	AccountHouse AccountKind = "HOUSE"
	// AccountProviderSettlement is a provider's settlement account: the leg
	// that says which external provider the money exchanges hands with.
	AccountProviderSettlement AccountKind = "PROVIDER_SETTLEMENT"
	// AccountRevenue is recognised house revenue (e.g. the margin retained on
	// a loss). Kept distinct from HOUSE so revenue can be reported separately.
	AccountRevenue AccountKind = "REVENUE"
)

// Valid reports whether k is a known account kind. Unknown kinds fail closed at
// construction so a typo can never silently create an unbalanced journal.
func (k AccountKind) Valid() bool {
	switch k {
	case AccountPlayerWallet, AccountHouse, AccountProviderSettlement, AccountRevenue:
		return true
	default:
		return false
	}
}

// String renders the kind for logs and error messages.
func (k AccountKind) String() string { return string(k) }

// Account is the identity of one account: its kind plus an optional reference
// (wallet id for a player wallet, provider id for a settlement account, empty
// for the singletons HOUSE/REVENUE).
type Account struct {
	kind AccountKind
	ref  string
}

// NewAccount builds an account. The reference rules are enforced here: a player
// wallet MUST carry a reference (its wallet id) and a provider settlement
// account MUST carry a provider id, while the singleton accounts must carry
// none. This keeps the chart of accounts well-formed by construction.
func NewAccount(kind AccountKind, ref string) (Account, error) {
	if !kind.Valid() {
		return Account{}, ErrInvalidAccount.With("unknown account kind %q", kind)
	}
	switch kind {
	case AccountPlayerWallet:
		if ref == "" {
			return Account{}, ErrInvalidAccount.With("%s requires a wallet reference", kind)
		}
		if _, err := uuid.Parse(ref); err != nil {
			return Account{}, ErrInvalidAccount.With("%s wallet reference is not a UUID: %q", kind, ref)
		}
	case AccountProviderSettlement:
		if ref == "" {
			return Account{}, ErrInvalidAccount.With("%s requires a provider reference", kind)
		}
	case AccountHouse, AccountRevenue:
		if ref != "" {
			return Account{}, ErrInvalidAccount.With("%s is a singleton and must not carry a reference (got %q)", kind, ref)
		}
	}
	return Account{kind: kind, ref: ref}, nil
}

// PlayerWalletAccount is the convenience constructor for a wallet's account.
func PlayerWalletAccount(walletID uuid.UUID) Account {
	return Account{kind: AccountPlayerWallet, ref: walletID.String()}
}

// HouseAccount is the singleton counterparty of every player movement: the
// operator's side of a wager. It carries no ref (there is one house).
func HouseAccount() Account { return Account{kind: AccountHouse} }

// Kind returns the account kind.
func (a Account) Kind() AccountKind { return a.kind }

// Ref returns the account reference ("" for singletons).
func (a Account) Ref() string { return a.ref }

// Key is a stable, sortable identity used for deduplication and ordering.
func (a Account) Key() string { return string(a.kind) + ":" + a.ref }

// String renders the account.
func (a Account) String() string { return a.Key() }

// Posting is one immutable leg of a journal entry.
type Posting struct {
	account   Account
	direction Direction
	amount    Money
}

// NewPosting builds a posting. Only the two real directions are accepted and
// the amount must be strictly positive: a zero or "no change" leg has no
// meaning in a journal and is rejected rather than silently dropped.
func NewPosting(account Account, direction Direction, amount Money) (Posting, error) {
	if direction != DirectionDebit && direction != DirectionCredit {
		return Posting{}, ErrLedgerArithmetic.With("posting direction %q is not a movement", direction)
	}
	if !amount.IsPositive() {
		return Posting{}, ErrLedgerArithmetic.With("posting amount must be positive, got %s", amount.String())
	}
	return Posting{account: account, direction: direction, amount: amount}, nil
}

// Account returns the posting's account.
func (p Posting) Account() Account { return p.account }

// Direction returns DEBIT or CREDIT.
func (p Posting) Direction() Direction { return p.direction }

// Amount returns the positive amount moved.
func (p Posting) Amount() Money { return p.amount }

// Signed returns the amount with its accounting sign (+credit, -debit), which is
// what the balancing check sums.
func (p Posting) Signed() Money {
	if p.direction == DirectionCredit {
		return p.amount
	}
	neg, _ := p.amount.Neg() // amount is positive; Neg cannot fail
	return neg
}

// JournalEntry is a balanced set of postings for exactly one transaction.
type JournalEntry struct {
	transactionID uuid.UUID
	currency      string
	postings      []Posting
	createdAt     time.Time
}

// NewJournalEntry builds and validates a journal entry.
//
// Rules, all enforced here:
//   - at least one posting (an entry that moves nothing is not a journal entry);
//   - every posting shares the entry currency (no cross-currency entry);
//   - the entry BALANCES: sum of credits == sum of debits, computed in Money so
//     an overflow is reported rather than wrapped;
//   - postings are stored sorted by account key with a STABLE sort, so the
//     representation is deterministic for a given input: equal-key legs (two
//     legs of one account) keep their relative order, and a rehydration that
//     feeds the stored order back in is byte-identical to the built entry.
func NewJournalEntry(transactionID uuid.UUID, currency string, postings []Posting, now time.Time) (JournalEntry, error) {
	if transactionID == uuid.Nil {
		return JournalEntry{}, ErrLedgerArithmetic.With("journal entry requires a transaction id")
	}
	if len(postings) == 0 {
		return JournalEntry{}, ErrUnbalancedJournal.With("entry for transaction %s carries no postings", transactionID)
	}
	for _, p := range postings {
		if p.amount.Currency() != currency {
			return JournalEntry{}, ErrCurrencyMismatch.With(
				"posting on %s is in %s, entry is in %s", p.account, p.amount.Currency(), currency)
		}
	}

	sorted := make([]Posting, len(postings))
	copy(sorted, postings)
	// SliceStable, not Slice: legs of the SAME account share a sort key, and
	// their relative order is meaningful — PlayerDelta accumulates over the
	// stored postings, so an unstable sort can change which intermediate sums
	// overflow. Stability makes the canonical order a function of the input
	// order alone, so the same legs always yield the same delta.
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].account.Key() < sorted[j].account.Key() })

	entry := JournalEntry{transactionID: transactionID, currency: currency, postings: sorted, createdAt: now}
	if !entry.Balanced() {
		return JournalEntry{}, ErrUnbalancedJournal.With(
			"entry for transaction %s: debits %s vs credits %s",
			transactionID, entry.TotalDebits().String(), entry.TotalCredits().String())
	}
	return entry, nil
}

// TransactionID returns the transaction this entry postings belong to.
func (e JournalEntry) TransactionID() uuid.UUID { return e.transactionID }

// Currency returns the entry currency.
func (e JournalEntry) Currency() string { return e.currency }

// Postings returns a copy of the postings, sorted by account key.
func (e JournalEntry) Postings() []Posting {
	out := make([]Posting, len(e.postings))
	copy(out, e.postings)
	return out
}

// CreatedAt returns the entry timestamp.
func (e JournalEntry) CreatedAt() time.Time { return e.createdAt }

func (e JournalEntry) total(dir Direction) Money {
	zero, _ := NewMoney(0, e.currency)
	sum := zero
	for _, p := range e.postings {
		if p.direction == dir {
			next, err := sum.Add(p.amount)
			if err != nil {
				// Amounts are bounded by Money's own limits; an overflow here is
				// a programming error, and returning the partial sum would hide
				// it. Panicking is deliberate and unrecoverable by design.
				panic(fmt.Sprintf("journal total overflow for transaction %s: %v", e.transactionID, err))
			}
			sum = next
		}
	}
	return sum
}

// TotalDebits returns the sum of the debit legs.
func (e JournalEntry) TotalDebits() Money { return e.total(DirectionDebit) }

// TotalCredits returns the sum of the credit legs.
func (e JournalEntry) TotalCredits() Money { return e.total(DirectionCredit) }

// Balanced reports whether debits equal credits. The zero-currency entry is
// never balanced unless it is empty, which NewJournalEntry already rejects.
func (e JournalEntry) Balanced() bool {
	d, c := e.TotalDebits(), e.TotalCredits()
	return d.Currency() == c.Currency() && d.AmountMinor() == c.AmountMinor()
}

// PlayerDelta returns the net effect of the entry on one player wallet account,
// signed (+credit, -debit). It is what a reconciliation compares against the
// wallet's stored balance movement.
func (e JournalEntry) PlayerDelta(walletID uuid.UUID) (Money, error) {
	target := PlayerWalletAccount(walletID)
	zero, _ := NewMoney(0, e.currency)
	sum := zero
	found := false
	for _, p := range e.postings {
		if p.account.Key() != target.Key() {
			continue
		}
		found = true
		next, err := sum.Add(p.Signed())
		if err != nil {
			return Money{}, err
		}
		sum = next
	}
	if !found {
		return Money{}, ErrInvalidAccount.With("entry %s has no postings for wallet %s", e.transactionID, walletID)
	}
	return sum, nil
}

// EntryForMovement builds the balanced journal entry for a wallet movement that
// was just applied. The counterparty is chosen from the operation kind:
//
//	BET   player debit  ↔ house credit      (the stake enters the house)
//	WIN   player credit ↔ house debit       (the house pays the win)
//	REFUND / ROLLBACK   mirrored: a refund of a BET debits the house back to
//	                    the player, so the counterparty is the house again
//	LOSS  no postings — a LOSS carries zero by domain rule and moves no money
//
// providerRef, when non-empty, adds the settlement leg is NOT synthesised here:
// the entry always has exactly the two value legs above (player + counterparty),
// which is what makes it balance. Settlement routing belongs to a later
// reporting step, not to the money-moving entry.
//
// The zero-amount LOSS case returns ok=false with a nil entry so the caller
// writes no journal row at all, mirroring how the ledger stays empty for it.
func EntryForMovement(
	walletID, transactionID uuid.UUID, kind TxKind, m Movement, now time.Time,
) (JournalEntry, bool, error) {
	if !m.HasChange {
		return JournalEntry{}, false, nil
	}
	if !m.Amount.IsPositive() {
		// Defensive: a Movement with HasChange but a non-positive amount cannot
		// be produced by Wallet.Debit/Credit, and must never reach the journal.
		return JournalEntry{}, false, ErrLedgerArithmetic.With("movement has no positive amount")
	}

	player := PlayerWalletAccount(walletID)
	house := HouseAccount()

	var debit, credit Account
	switch m.Direction {
	case DirectionDebit:
		// The player's side. Who receives the value depends on the kind: a BET's
		// stake goes to the house; a refund/rollback of a BET also settles
		// against the house. Either way the counterparty is the house here.
		debit, credit = player, house
	case DirectionCredit:
		debit, credit = house, player
	default:
		return JournalEntry{}, false, ErrLedgerArithmetic.With("movement direction %q is not a movement", m.Direction)
	}

	debitLeg, err := NewPosting(debit, DirectionDebit, m.Amount)
	if err != nil {
		return JournalEntry{}, false, err
	}
	creditLeg, err := NewPosting(credit, DirectionCredit, m.Amount)
	if err != nil {
		return JournalEntry{}, false, err
	}
	entry, err := NewJournalEntry(transactionID, m.Amount.Currency(), []Posting{debitLeg, creditLeg}, now)
	if err != nil {
		return JournalEntry{}, false, err
	}
	return entry, true, nil
}
