package domain

import (
	"time"

	"github.com/google/uuid"
)

// Direction of a ledger movement.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
	DirectionNone   Direction = "NONE"
)

func (d Direction) Valid() bool {
	return d == DirectionDebit || d == DirectionCredit || d == DirectionNone
}

// Movement is the financial effect of a wallet state change. The aggregate
// produces it; the persistence layer turns it into a ledger entry together
// with the balance update, inside the same SQL transaction.
type Movement struct {
	HasChange     bool
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	WalletVersion int64 // version AFTER the change
}

// Wallet is the financial aggregate root. Its balance can only change
// through Credit/Debit, which preserve the invariants:
//   - balance >= 0 at all times;
//   - movement currency must match wallet currency;
//   - version starts at 1 and increases only when the balance changes.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	currency  string
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// OpenWallet creates a new wallet with an initial balance (zero allowed).
// If the initial balance is positive, the returned Movement carries the
// opening credit; otherwise Movement.HasChange is false and no ledger entry
// is produced.
func OpenWallet(playerID uuid.UUID, currency string, initial Money, now time.Time) (*Wallet, Movement, error) {
	if err := ValidateCurrency(currency); err != nil {
		return nil, Movement{}, err
	}
	if initial.Currency() != currency {
		return nil, Movement{}, ErrCurrencyMismatch.With("initial balance %s vs wallet %s", initial.Currency(), currency)
	}
	if initial.IsNegative() {
		return nil, Movement{}, ErrInvalidAmount.With("initial balance cannot be negative")
	}
	id, err := NewID()
	if err != nil {
		return nil, Movement{}, err
	}
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  currency,
		balance:   initial,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}
	mv := Movement{HasChange: false, BalanceBefore: initial, BalanceAfter: initial, WalletVersion: 1}
	if initial.IsPositive() {
		mv = Movement{HasChange: true, Direction: DirectionCredit, Amount: initial,
			BalanceBefore: ZeroMoneyMust(currency), BalanceAfter: initial, WalletVersion: 1}
	}
	return w, mv, nil
}

// RehydrateWallet rebuilds an aggregate from persisted state without
// reapplying movements or re-emitting events.
func RehydrateWallet(id, playerID uuid.UUID, currency string, balanceMinor, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if err := ValidateCurrency(currency); err != nil {
		return nil, err
	}
	if version < 1 {
		return nil, ErrZeroVersion
	}
	balance, err := NewMoney(balanceMinor, currency)
	if err != nil {
		return nil, err
	}
	if balance.IsNegative() {
		return nil, ErrInvalidAmount.With("rehydrated balance is negative")
	}
	return &Wallet{id: id, playerID: playerID, currency: currency, balance: balance,
		version: version, createdAt: createdAt, updatedAt: updatedAt}, nil
}

func (w *Wallet) ID() uuid.UUID        { return w.id }
func (w *Wallet) PlayerID() uuid.UUID  { return w.playerID }
func (w *Wallet) Currency() string     { return w.currency }
func (w *Wallet) Balance() Money       { return w.balance }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Credit adds a positive amount in the wallet currency.
func (w *Wallet) Credit(m Money, now time.Time) (Movement, error) {
	return w.apply(m, DirectionCredit, now)
}

// Debit removes a positive amount, refusing to go below zero.
func (w *Wallet) Debit(m Money, now time.Time) (Movement, error) {
	if m.Currency() != w.currency {
		return Movement{}, ErrCurrencyMismatch.With("movement %s vs wallet %s", m.Currency(), w.currency)
	}
	if !m.IsPositive() {
		return Movement{}, ErrInvalidAmount.With("movement must be positive, got %s", m.String())
	}
	after, err := w.balance.Sub(m)
	if err != nil {
		return Movement{}, err
	}
	if after.IsNegative() {
		return Movement{}, ErrInsufficientFunds.With("balance %s, requested %s", w.balance.String(), m.String())
	}
	before := w.balance
	w.balance = after
	w.version++
	w.updatedAt = now
	return Movement{HasChange: true, Direction: DirectionDebit, Amount: m,
		BalanceBefore: before, BalanceAfter: after, WalletVersion: w.version}, nil
}

func (w *Wallet) apply(m Money, dir Direction, now time.Time) (Movement, error) {
	if !m.IsPositive() {
		return Movement{}, ErrInvalidAmount.With("movement must be positive, got %s", m.String())
	}
	if m.Currency() != w.currency {
		return Movement{}, ErrCurrencyMismatch.With("movement %s vs wallet %s", m.Currency(), w.currency)
	}
	var after Money
	var err error
	if dir == DirectionCredit {
		after, err = w.balance.Add(m)
	} else {
		after, err = w.balance.Sub(m)
	}
	if err != nil {
		return Movement{}, err
	}
	before := w.balance
	w.balance = after
	w.version++
	w.updatedAt = now
	return Movement{HasChange: true, Direction: dir, Amount: m,
		BalanceBefore: before, BalanceAfter: after, WalletVersion: w.version}, nil
}

// ZeroMoneyMust is a convenience for places where the currency was already
// validated (it panics only on a programmer error, never on business input).
func ZeroMoneyMust(currency string) Money {
	m, err := ZeroMoney(currency)
	if err != nil {
		panic(err)
	}
	return m
}
