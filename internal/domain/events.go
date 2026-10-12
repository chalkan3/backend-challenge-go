package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event type names used in the outbox envelope.
const (
	EventWagerTransactionProcessed        = "WagerTransactionProcessed"
	EventWagerTransactionRejected         = "WagerTransactionRejected"
	EventWalletBalanceChanged             = "WalletBalanceChanged"
	EventWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// Aggregate types.
const (
	AggregateWagerTransaction = "wager_transaction"
	AggregateWallet           = "wallet"
)

// EventSchemaVersion is the envelope "version" set by event constructors.
const EventSchemaVersion = 1

// MoneyPayload is the wire representation of Money: a decimal string plus
// the currency. Never a float.
type MoneyPayload struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func NewMoneyPayload(m Money) MoneyPayload {
	return MoneyPayload{Amount: m.String(), Currency: m.Currency()}
}

// Envelope is the integration event envelope. Data is a typed, immutable
// snapshot; republications preserve eventId.
type Envelope struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateType string    `json:"aggregateType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId,omitempty"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          any       `json:"data"`
}

type WagerTransactionProcessedData struct {
	TransactionID         string        `json:"transactionId"`
	Kind                  string        `json:"kind"`
	ProviderID            string        `json:"providerId,omitempty"`
	ExternalTransactionID string        `json:"externalTransactionId,omitempty"`
	PlayerID              string        `json:"playerId"`
	WalletID              string        `json:"walletId"`
	RoundID               string        `json:"roundId,omitempty"`
	GameID                string        `json:"gameId,omitempty"`
	Money                 MoneyPayload  `json:"money"`
	ResultDirection       string        `json:"resultDirection"`
	BalanceAfter          *MoneyPayload `json:"balanceAfter,omitempty"`
	WalletVersion         int64         `json:"walletVersion"`
}

type WagerTransactionRejectedData struct {
	TransactionID         string       `json:"transactionId"`
	Kind                  string       `json:"kind"`
	ProviderID            string       `json:"providerId,omitempty"`
	ExternalTransactionID string       `json:"externalTransactionId,omitempty"`
	PlayerID              string       `json:"playerId"`
	WalletID              string       `json:"walletId"`
	Money                 MoneyPayload `json:"money"`
	FailureCode           string       `json:"failureCode"`
	FailureMessage        string       `json:"failureMessage"`
}

type WalletBalanceChangedData struct {
	WalletID      string `json:"walletId"`
	TransactionID string `json:"transactionId"`
	// ExternalTransactionID echoes the provider's external id of the causing
	// operation (empty for internal openings), letting consumers correlate
	// balance changes with their own requests.
	ExternalTransactionID string       `json:"externalTransactionId,omitempty"`
	Direction             string       `json:"direction"`
	Money                 MoneyPayload `json:"money"`
	BalanceBefore         MoneyPayload `json:"balanceBefore"`
	BalanceAfter          MoneyPayload `json:"balanceAfter"`
	WalletVersion         int64        `json:"walletVersion"`
}

type WagerTransactionPendingReferenceData struct {
	TransactionID                  string       `json:"transactionId"`
	Kind                           string       `json:"kind"`
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	Money                          MoneyPayload `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId"`
	Attempts                       int          `json:"attempts"`
}

// newEnvelope builds the shared envelope with a fresh UUIDv7 event id and
// UTC occurrence time. It returns an error instead of panicking.
func newEnvelope(eventType, aggregateType string, aggregateID uuid.UUID, correlationID, causationID string, data any) (Envelope, error) {
	id, err := NewID()
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		EventID:       id.String(),
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID.String(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    time.Now().UTC(),
		Version:       EventSchemaVersion,
		Data:          data,
	}, nil
}

func NewWagerTransactionProcessedEvent(t *WagerTransaction) (Envelope, error) {
	data := WagerTransactionProcessedData{
		TransactionID:         t.ID().String(),
		Kind:                  string(t.Kind()),
		ProviderID:            t.ProviderID(),
		ExternalTransactionID: t.ExternalTxID(),
		PlayerID:              t.PlayerID().String(),
		WalletID:              t.WalletID().String(),
		RoundID:               t.RoundID(),
		GameID:                t.GameID(),
		Money:                 NewMoneyPayload(t.Amount()),
		ResultDirection:       string(t.ResultDirection()),
		WalletVersion:         derefInt64(t.WalletVersionAfter()),
	}
	if b := t.BalanceAfter(); b != nil {
		p := NewMoneyPayload(*b)
		data.BalanceAfter = &p
	}
	return newEnvelope(EventWagerTransactionProcessed, AggregateWagerTransaction, t.ID(), t.CorrelationID(), "", data)
}

func NewWagerTransactionRejectedEvent(t *WagerTransaction) (Envelope, error) {
	return newEnvelope(EventWagerTransactionRejected, AggregateWagerTransaction, t.ID(), t.CorrelationID(), "", WagerTransactionRejectedData{
		TransactionID:         t.ID().String(),
		Kind:                  string(t.Kind()),
		ProviderID:            t.ProviderID(),
		ExternalTransactionID: t.ExternalTxID(),
		PlayerID:              t.PlayerID().String(),
		WalletID:              t.WalletID().String(),
		Money:                 NewMoneyPayload(t.Amount()),
		FailureCode:           t.FailureCode(),
		FailureMessage:        t.FailureMessage(),
	})
}

func NewWalletBalanceChangedEvent(t *WagerTransaction, m Movement) (Envelope, error) {
	return newEnvelope(EventWalletBalanceChanged, AggregateWallet, t.WalletID(), t.CorrelationID(), t.ID().String(), WalletBalanceChangedData{
		WalletID:              t.WalletID().String(),
		TransactionID:         t.ID().String(),
		ExternalTransactionID: t.ExternalTxID(),
		Direction:             string(m.Direction),
		Money:                 NewMoneyPayload(m.Amount),
		BalanceBefore:         NewMoneyPayload(m.BalanceBefore),
		BalanceAfter:          NewMoneyPayload(m.BalanceAfter),
		WalletVersion:         m.WalletVersion,
	})
}

func NewWagerTransactionPendingReferenceEvent(t *WagerTransaction) (Envelope, error) {
	return newEnvelope(EventWagerTransactionPendingReference, AggregateWagerTransaction, t.ID(), t.CorrelationID(), "", WagerTransactionPendingReferenceData{
		TransactionID:                  t.ID().String(),
		Kind:                           string(t.Kind()),
		ProviderID:                     t.ProviderID(),
		ExternalTransactionID:          t.ExternalTxID(),
		PlayerID:                       t.PlayerID().String(),
		WalletID:                       t.WalletID().String(),
		Money:                          NewMoneyPayload(t.Amount()),
		ReferenceExternalTransactionID: t.ReferenceExternalTxID(),
		Attempts:                       t.PendingAttempts(),
	})
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// MarshalEnvelope serializes an outbox envelope to JSON (its durable payload).
func MarshalEnvelope(e Envelope) ([]byte, error) {
	return json.Marshal(e)
}

// EventWagerTransactionRequested is the inbound SQS event consumed from
// wager-transactions.fifo. Its messageId (envelope level) is the durable
// identity verified against the inbox on redelivery.
const EventWagerTransactionRequested = "WagerTransactionRequested"

// WagerTransactionRequestedData is the business payload of the inbound event.
type WagerTransactionRequestedData struct {
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	IdempotencyKey                 string       `json:"idempotencyKey,omitempty"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Money                          MoneyPayload `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
}
