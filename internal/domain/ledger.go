package domain

import (
	"time"

	"github.com/google/uuid"
)

// LedgerEntry is one immutable, append-only financial movement line.
// Invariants enforced at construction and by the database:
//   - amount > 0;
//   - balanceAfter = balanceBefore - amount (DEBIT) or + amount (CREDIT);
//   - balanceAfter >= 0;
//   - (walletId, transactionId) is unique.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	currency      string
	amount        Money
	balanceBefore Money
	balanceAfter  Money
	createdAt     time.Time
}

// NewLedgerEntry builds the ledger line for a wallet movement.
func NewLedgerEntry(walletID, transactionID uuid.UUID, m Movement, now time.Time) (LedgerEntry, error) {
	if !m.HasChange || (m.Direction != DirectionDebit && m.Direction != DirectionCredit) {
		return LedgerEntry{}, ErrLedgerArithmetic.With("movement has no applicable direction")
	}
	if !m.Amount.IsPositive() {
		return LedgerEntry{}, ErrLedgerArithmetic.With("ledger amount must be positive, got %s", m.Amount.String())
	}
	var expect Money
	var err error
	if m.Direction == DirectionDebit {
		expect, err = m.BalanceBefore.Sub(m.Amount)
	} else {
		expect, err = m.BalanceBefore.Add(m.Amount)
	}
	if err != nil {
		return LedgerEntry{}, ErrLedgerArithmetic.With("arithmetic check failed: %v", err)
	}
	if d, err := expect.Compare(m.BalanceAfter); err != nil || d != 0 {
		return LedgerEntry{}, ErrLedgerArithmetic.With("balanceAfter %s does not match %s %s %s",
			m.BalanceAfter.String(), m.BalanceBefore.String(), m.Direction, m.Amount.String())
	}
	if m.BalanceAfter.IsNegative() {
		return LedgerEntry{}, ErrLedgerArithmetic.With("ledger would record a negative balance")
	}
	id, err := NewID()
	if err != nil {
		return LedgerEntry{}, err
	}
	return LedgerEntry{
		id: id, walletID: walletID, transactionID: transactionID,
		direction: m.Direction, currency: m.Amount.Currency(), amount: m.Amount,
		balanceBefore: m.BalanceBefore, balanceAfter: m.BalanceAfter, createdAt: now,
	}, nil
}

func (e LedgerEntry) ID() uuid.UUID            { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID      { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID { return e.transactionID }
func (e LedgerEntry) Direction() Direction     { return e.direction }
func (e LedgerEntry) Currency() string         { return e.currency }
func (e LedgerEntry) Amount() Money            { return e.amount }
func (e LedgerEntry) BalanceBefore() Money     { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() Money      { return e.balanceAfter }
func (e LedgerEntry) CreatedAt() time.Time     { return e.createdAt }

// LedgerCursor is the opaque pagination cursor: (createdAt, id) keyset.
type LedgerCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// LedgerEntryParams carries the persisted representation for rehydration.
type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	CreatedAt     time.Time
}

// RehydrateLedgerEntry rebuilds a persisted ledger line without revalidating
// the movement that produced it (that state is immutable and already committed).
func RehydrateLedgerEntry(p LedgerEntryParams) (*LedgerEntry, error) {
	if p.Direction != DirectionDebit && p.Direction != DirectionCredit {
		return nil, ErrLedgerArithmetic.With("invalid persisted direction %q", p.Direction)
	}
	return &LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		currency:      p.Amount.Currency(),
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		createdAt:     p.CreatedAt,
	}, nil
}
