// Package domain_test exercises the integration-event envelope builders,
// with testify.
package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// processedTx builds a transaction in the PROCESSED state so the event
// factories that require a financial result can run.
func processedTx(t *testing.T) (*domain.WagerTransaction, domain.Movement) {
	t.Helper()
	tx := externalTx(t, func(p *domain.ExternalTxParams) {
		p.Money = mustParse(t, "10.00", "BRL")
	})
	mv := domain.Movement{
		HasChange: true, Direction: domain.DirectionDebit,
		Amount:        mustParse(t, "10.00", "BRL"),
		BalanceBefore: mustParse(t, "50.00", "BRL"),
		BalanceAfter:  mustParse(t, "40.00", "BRL"),
		WalletVersion: 2,
	}
	require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
		Direction: mv.Direction, ResultAmount: mv.Amount,
		BalanceBefore: mv.BalanceBefore, BalanceAfter: mv.BalanceAfter,
		WalletVersion: mv.WalletVersion,
	}, baseTime))
	return tx, mv
}

func TestWagerTransactionProcessedEvent(t *testing.T) {
	tx, _ := processedTx(t)

	e, err := domain.NewWagerTransactionProcessedEvent(tx)
	require.NoError(t, err)

	assert.Equal(t, domain.EventWagerTransactionProcessed, e.EventType)
	assert.Equal(t, domain.AggregateWagerTransaction, e.AggregateType)
	assert.Equal(t, tx.ID().String(), e.AggregateID)
	assert.NotEmpty(t, e.EventID)
	assert.Equal(t, domain.EventSchemaVersion, e.Version)
	assert.Equal(t, "corr-1", e.CorrelationID)

	// The data snapshot must be JSON-safe and carry the money as a decimal
	// string, never as a float.
	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "amountMinor")
	assert.Contains(t, string(b), `"amount":"10.00"`)
}

func TestWalletBalanceChangedEvent(t *testing.T) {
	tx, mv := processedTx(t)

	e, err := domain.NewWalletBalanceChangedEvent(tx, mv)
	require.NoError(t, err)

	assert.Equal(t, domain.EventWalletBalanceChanged, e.EventType)
	assert.Equal(t, domain.AggregateWallet, e.AggregateType)
	assert.Equal(t, tx.WalletID().String(), e.AggregateID)

	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"balanceBefore":{"amount":"50.00","currency":"BRL"}`)
	assert.Contains(t, string(b), `"balanceAfter":{"amount":"40.00","currency":"BRL"}`)
}

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	tx, _ := processedTx(t)

	e, err := domain.NewWagerTransactionProcessedEvent(tx)
	require.NoError(t, err)

	b, err := domain.MarshalEnvelope(e)
	require.NoError(t, err)

	var got domain.Envelope
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, e.EventID, got.EventID, "eventId is assigned once by the outbox and must survive serialization")
	assert.Equal(t, e.EventType, got.EventType)
	assert.Equal(t, e.AggregateID, got.AggregateID)
	assert.Equal(t, e.Version, got.Version)
}
