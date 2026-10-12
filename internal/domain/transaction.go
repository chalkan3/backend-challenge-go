package domain

import (
	"time"

	"github.com/google/uuid"
)

// TxKind enumerates the wager transaction kinds.
type TxKind string

const (
	KindOpening  TxKind = "OPENING"
	KindBet      TxKind = "BET"
	KindWin      TxKind = "WIN"
	KindLoss     TxKind = "LOSS"
	KindRefund   TxKind = "REFUND"
	KindRollback TxKind = "ROLLBACK"
)

func ParseTxKind(s string) (TxKind, error) {
	switch TxKind(s) {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return TxKind(s), nil
	default:
		return "", ErrInvalidKind.With("unknown kind %q", s)
	}
}

func (k TxKind) IsExternal() bool {
	return k != KindOpening
}

// TxStatus is the state-machine state of a wager transaction.
type TxStatus string

const (
	StatusPending          TxStatus = "PENDING"
	StatusPendingReference TxStatus = "PENDING_REFERENCE"
	StatusProcessed        TxStatus = "PROCESSED"
	StatusRejected         TxStatus = "REJECTED"
	StatusFailed           TxStatus = "FAILED"
)

// TxOrigin distinguishes internal openings from external provider operations.
type TxOrigin string

const (
	OriginInternal TxOrigin = "INTERNAL"
	OriginExternal TxOrigin = "EXTERNAL"
)

// ReceivedVia records the entry channel of an operation.
type ReceivedVia string

const (
	ViaHTTP ReceivedVia = "HTTP"
	ViaSQS  ReceivedVia = "SQS"
)

// allowedTransitions is the documented state machine:
//
//	PENDING -> PENDING_REFERENCE | PROCESSED | REJECTED | FAILED
//	PENDING_REFERENCE -> PENDING_REFERENCE (retry) | PROCESSED | REJECTED | FAILED
//	PROCESSED | REJECTED | FAILED are terminal.
//
// Synchronous operations without dependencies go straight from PENDING (in
// memory) to a terminal state inside a single SQL transaction, with no
// intermediate committed PENDING row.
var allowedTransitions = map[TxStatus][]TxStatus{
	StatusPending:          {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
	StatusPendingReference: {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
	StatusProcessed:        {},
	StatusRejected:         {},
	StatusFailed:           {},
}

// ExternalTxParams carries the externally supplied business fields.
type ExternalTxParams struct {
	ProviderID            string
	ExternalTransactionID string
	PlayerID              uuid.UUID
	WalletID              uuid.UUID
	RoundID               string
	GameID                string
	Kind                  TxKind
	Money                 Money
	ReferenceExternalTxID string // required for REFUND/ROLLBACK
	ReceivedVia           ReceivedVia
	CorrelationID         string
	Now                   time.Time
}

// WagerTransaction is the audit record of one wallet operation, external or
// internal (OPENING). It carries the persisted financial result used by
// idempotent replays: the balance observed at processing time.
type WagerTransaction struct {
	id                    uuid.UUID
	kind                  TxKind
	origin                TxOrigin
	providerID            *string
	externalTxID          *string
	idempotencyKey        *string
	payloadHash           *string
	playerID              uuid.UUID
	walletID              uuid.UUID
	roundID               *string
	gameID                *string
	currency              string
	amount                Money
	referenceExternalTxID *string
	referenceTxID         *uuid.UUID
	status                TxStatus
	failureCode           *string
	failureMessage        *string
	resultDirection       Direction
	resultAmount          Money
	balanceBefore         *Money
	balanceAfter          *Money
	walletVersionAfter    *int64
	correlationID         string
	receivedVia           ReceivedVia
	pendingAttempts       int
	nextRetryAt           *time.Time
	createdAt             time.Time
	updatedAt             time.Time
}

// NewExternalTransaction validates the business rules of an incoming external
// operation and returns a PENDING transaction (not yet persisted).
func NewExternalTransaction(p ExternalTxParams) (*WagerTransaction, error) {
	// The shape of an external operation is validated by a composed
	// Specification chain (see spec_transaction.go). Rules are evaluated in a
	// fixed order so the most fundamental error wins: kind identity before
	// missing metadata, amount policy before reference requirements. This
	// keeps the published failure codes stable while making each rule an
	// individually testable, named value.
	if err := externalTransactionSpecification.IsSatisfiedBy(p); err != nil {
		return nil, err
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	provider, external, key := p.ProviderID, p.ExternalTransactionID, ""
	ref := p.ReferenceExternalTxID
	t := &WagerTransaction{
		id: id, kind: p.Kind, origin: OriginExternal,
		providerID: &provider, externalTxID: &external, idempotencyKey: &key,
		playerID: p.PlayerID, walletID: p.WalletID,
		currency: p.Money.Currency(), amount: p.Money,
		status:          StatusPending,
		resultDirection: DirectionNone,
		resultAmount:    ZeroMoneyMust(p.Money.Currency()),
		correlationID:   p.CorrelationID, receivedVia: p.ReceivedVia,
		createdAt: p.Now, updatedAt: p.Now,
	}
	if p.RoundID != "" {
		t.roundID = &p.RoundID
	}
	if p.GameID != "" {
		t.gameID = &p.GameID
	}
	if ref != "" {
		t.referenceExternalTxID = &ref
	}
	return t, nil
}

// NewOpeningTransaction builds the internal OPENING transaction that
// accompanies wallet creation (immediately marked PROCESSED by the service).
func NewOpeningTransaction(playerID, walletID uuid.UUID, currency string, initial Money, now time.Time) (*WagerTransaction, error) {
	if err := ValidateCurrency(currency); err != nil {
		return nil, err
	}
	if initial.Currency() != currency {
		return nil, ErrCurrencyMismatch.With("initial %s vs wallet %s", initial.Currency(), currency)
	}
	if initial.IsNegative() {
		return nil, ErrInvalidAmount.With("opening balance cannot be negative")
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	return &WagerTransaction{
		id: id, kind: KindOpening, origin: OriginInternal,
		playerID: playerID, walletID: walletID, currency: currency, amount: initial,
		status: StatusPending, resultDirection: DirectionNone,
		resultAmount:  ZeroMoneyMust(currency),
		correlationID: "wallet-opening", receivedVia: ViaHTTP,
		createdAt: now, updatedAt: now,
	}, nil
}

// RehydrateParams is the persisted representation used to rebuild a
// transaction without reapplying anything.
type RehydrateParams struct {
	ID                    uuid.UUID
	Kind                  TxKind
	Origin                TxOrigin
	ProviderID            *string
	ExternalTxID          *string
	IdempotencyKey        *string
	PayloadHash           *string
	PlayerID              uuid.UUID
	WalletID              uuid.UUID
	RoundID               *string
	GameID                *string
	Currency              string
	AmountMinor           int64
	ReferenceExternalTxID *string
	ReferenceTxID         *uuid.UUID
	Status                TxStatus
	FailureCode           *string
	FailureMessage        *string
	ResultDirection       Direction
	ResultAmountMinor     int64
	BalanceBeforeMinor    *int64
	BalanceAfterMinor     *int64
	WalletVersionAfter    *int64
	CorrelationID         string
	ReceivedVia           ReceivedVia
	PendingAttempts       int
	NextRetryAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func RehydrateWagerTransaction(p RehydrateParams) (*WagerTransaction, error) {
	amount, err := NewMoney(p.AmountMinor, p.Currency)
	if err != nil {
		return nil, err
	}
	resultAmount, err := NewMoney(p.ResultAmountMinor, p.Currency)
	if err != nil {
		return nil, err
	}
	moneyPtr := func(v *int64) *Money {
		if v == nil {
			return nil
		}
		m, err := NewMoney(*v, p.Currency)
		if err != nil {
			panic(err) // persisted data violates currency invariant: programming/data error
		}
		return &m
	}
	return &WagerTransaction{
		id: p.ID, kind: p.Kind, origin: p.Origin,
		providerID: p.ProviderID, externalTxID: p.ExternalTxID,
		idempotencyKey: p.IdempotencyKey, payloadHash: p.PayloadHash,
		playerID: p.PlayerID, walletID: p.WalletID,
		roundID: p.RoundID, gameID: p.GameID,
		currency: p.Currency, amount: amount,
		referenceExternalTxID: p.ReferenceExternalTxID, referenceTxID: p.ReferenceTxID,
		status: p.Status, failureCode: p.FailureCode, failureMessage: p.FailureMessage,
		resultDirection: p.ResultDirection, resultAmount: resultAmount,
		balanceBefore: moneyPtr(p.BalanceBeforeMinor), balanceAfter: moneyPtr(p.BalanceAfterMinor),
		walletVersionAfter: p.WalletVersionAfter,
		correlationID:      p.CorrelationID, receivedVia: p.ReceivedVia,
		pendingAttempts: p.PendingAttempts, nextRetryAt: p.NextRetryAt,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt,
	}, nil
}

// SetIdempotency attaches the idempotency key and deterministic payload hash
// (both computed by the application layer, persisted for dedup/conflict).
func (t *WagerTransaction) SetIdempotency(key, payloadHash string) {
	k, h := key, payloadHash
	t.idempotencyKey, t.payloadHash = &k, &h
}

func (t *WagerTransaction) canTransition(to TxStatus) error {
	for _, s := range allowedTransitions[t.status] {
		if s == to {
			return nil
		}
	}
	return ErrInvalidTransition.With("%s -> %s", t.status, to)
}

// MarkPendingReference moves the transaction to PENDING_REFERENCE (waiting
// for a referenced transaction), scheduling the next retry with backoff.
func (t *WagerTransaction) MarkPendingReference(nextRetry time.Time, now time.Time) error {
	if err := t.canTransition(StatusPendingReference); err != nil {
		return err
	}
	t.status = StatusPendingReference
	t.pendingAttempts++
	retry := nextRetry
	t.nextRetryAt = &retry
	t.updatedAt = now
	return nil
}

// RetryPendingReference reschedules a waiting transaction without leaving
// PENDING_REFERENCE (used by the durable reference worker).
func (t *WagerTransaction) RetryPendingReference(nextRetry time.Time) error {
	if t.status != StatusPendingReference {
		return ErrInvalidTransition.With("%s -> %s", t.status, StatusPendingReference)
	}
	t.pendingAttempts++
	retry := nextRetry
	t.nextRetryAt = &retry
	return nil
}

// ProcessedResult is the financial outcome committed with the transaction.
type ProcessedResult struct {
	Direction     Direction
	ResultAmount  Money
	BalanceBefore Money
	BalanceAfter  Money
	WalletVersion int64 // wallet version after the change
}

// MarkProcessed transitions to PROCESSED, recording the financial result
// observed at processing time (returned verbatim on idempotent replays).
func (t *WagerTransaction) MarkProcessed(r ProcessedResult, now time.Time) error {
	if err := t.canTransition(StatusProcessed); err != nil {
		return err
	}
	if r.Direction != DirectionNone && !r.Direction.Valid() {
		return ErrInvalidTransaction.With("invalid result direction %q", r.Direction)
	}
	if r.Direction == DirectionNone && !r.ResultAmount.IsZero() {
		return ErrLedgerArithmetic.With("no-movement result must carry a zero amount")
	}
	if t.currency != r.ResultAmount.Currency() ||
		t.currency != r.BalanceBefore.Currency() ||
		t.currency != r.BalanceAfter.Currency() {
		return ErrCurrencyMismatch.With("result currency does not match transaction currency %s", t.currency)
	}
	t.status = StatusProcessed
	t.resultDirection = r.Direction
	t.resultAmount = r.ResultAmount
	before, after := r.BalanceBefore, r.BalanceAfter
	t.balanceBefore, t.balanceAfter = &before, &after
	version := r.WalletVersion
	t.walletVersionAfter = &version
	t.updatedAt = now
	return nil
}

// MarkRejected transitions to REJECTED with a stable failure code.
func (t *WagerTransaction) MarkRejected(code, message string, now time.Time) error {
	return t.markTerminal(StatusRejected, code, message, now)
}

// MarkFailed transitions to FAILED (permanent infrastructure failure kept
// for audit).
func (t *WagerTransaction) MarkFailed(code, message string, now time.Time) error {
	return t.markTerminal(StatusFailed, code, message, now)
}

func (t *WagerTransaction) markTerminal(status TxStatus, code, message string, now time.Time) error {
	if err := t.canTransition(status); err != nil {
		return err
	}
	if code == "" {
		return ErrInvalidTransaction.With("terminal transitions require a failure code")
	}
	t.status = status
	c, m := code, message
	t.failureCode, t.failureMessage = &c, &m
	t.updatedAt = now
	return nil
}

func (t *WagerTransaction) Terminal() bool { return len(allowedTransitions[t.status]) == 0 }

// Terminal reports whether a status is terminal on its own.
func (s TxStatus) Terminal() bool { return len(allowedTransitions[s]) == 0 }

func (t *WagerTransaction) ID() uuid.UUID              { return t.id }
func (t *WagerTransaction) Kind() TxKind               { return t.kind }
func (t *WagerTransaction) Origin() TxOrigin           { return t.origin }
func (t *WagerTransaction) ProviderID() string         { return derefString(t.providerID) }
func (t *WagerTransaction) ProviderIDPtr() *string     { return t.providerID }
func (t *WagerTransaction) ExternalTxID() string       { return derefString(t.externalTxID) }
func (t *WagerTransaction) ExternalTxIDPtr() *string   { return t.externalTxID }
func (t *WagerTransaction) IdempotencyKey() string     { return derefString(t.idempotencyKey) }
func (t *WagerTransaction) IdempotencyKeyPtr() *string { return t.idempotencyKey }
func (t *WagerTransaction) PayloadHash() string        { return derefString(t.payloadHash) }
func (t *WagerTransaction) PayloadHashPtr() *string    { return t.payloadHash }
func (t *WagerTransaction) PlayerID() uuid.UUID        { return t.playerID }
func (t *WagerTransaction) WalletID() uuid.UUID        { return t.walletID }
func (t *WagerTransaction) RoundID() string            { return derefString(t.roundID) }
func (t *WagerTransaction) RoundIDPtr() *string        { return t.roundID }
func (t *WagerTransaction) GameID() string             { return derefString(t.gameID) }
func (t *WagerTransaction) GameIDPtr() *string         { return t.gameID }
func (t *WagerTransaction) Currency() string           { return t.currency }
func (t *WagerTransaction) Amount() Money              { return t.amount }
func (t *WagerTransaction) ReferenceExternalTxID() string {
	return derefString(t.referenceExternalTxID)
}
func (t *WagerTransaction) ReferenceExternalTxIDPtr() *string { return t.referenceExternalTxID }
func (t *WagerTransaction) ReferenceTxID() *uuid.UUID         { return t.referenceTxID }

// SetReference links a REFUND/ROLLBACK to the resolved internal reference.
func (t *WagerTransaction) SetReference(id uuid.UUID) error {
	if t.kind != KindRefund && t.kind != KindRollback {
		return ErrInvalidKind.With("only REFUND/ROLLBACK carry an internal reference")
	}
	t.referenceTxID = &id
	return nil
}
func (t *WagerTransaction) Status() TxStatus           { return t.status }
func (t *WagerTransaction) FailureCode() string        { return derefString(t.failureCode) }
func (t *WagerTransaction) FailureCodePtr() *string    { return t.failureCode }
func (t *WagerTransaction) FailureMessage() string     { return derefString(t.failureMessage) }
func (t *WagerTransaction) FailureMessagePtr() *string { return t.failureMessage }
func (t *WagerTransaction) ResultDirection() Direction { return t.resultDirection }
func (t *WagerTransaction) ResultAmount() Money        { return t.resultAmount }
func (t *WagerTransaction) BalanceBefore() *Money      { return t.balanceBefore }
func (t *WagerTransaction) BalanceAfter() *Money       { return t.balanceAfter }
func (t *WagerTransaction) WalletVersionAfter() *int64 { return t.walletVersionAfter }
func (t *WagerTransaction) CorrelationID() string      { return t.correlationID }
func (t *WagerTransaction) ReceivedVia() ReceivedVia   { return t.receivedVia }
func (t *WagerTransaction) PendingAttempts() int       { return t.pendingAttempts }
func (t *WagerTransaction) NextRetryAt() *time.Time    { return t.nextRetryAt }
func (t *WagerTransaction) CreatedAt() time.Time       { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time       { return t.updatedAt }

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
