// Package domain_test exercises the WagerTransaction state machine and its
// validation rules through the exported API, with testify.
package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func externalTx(t *testing.T, mutate func(*domain.ExternalTxParams)) *domain.WagerTransaction {
	t.Helper()
	p := domain.ExternalTxParams{
		ProviderID:            "provider-a",
		ExternalTransactionID: "ext-1",
		PlayerID:              domain.MustNewID(),
		WalletID:              domain.MustNewID(),
		Kind:                  domain.KindBet,
		Money:                 mustParse(t, "10.00", "BRL"),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "corr-1",
		Now:                   baseTime,
	}
	if mutate != nil {
		mutate(&p)
	}
	tx, err := domain.NewExternalTransaction(p)
	require.NoError(t, err)
	return tx
}

func TestNewExternalTransaction(t *testing.T) {
	t.Run("external kinds default to PENDING", func(t *testing.T) {
		tx := externalTx(t, nil)
		assert.Equal(t, domain.StatusPending, tx.Status())
		assert.Equal(t, domain.KindBet, tx.Kind())
		assert.True(t, tx.Kind().IsExternal())
		assert.Equal(t, domain.OriginExternal, tx.Origin())
		assert.Equal(t, domain.ViaHTTP, tx.ReceivedVia())
		assert.Equal(t, "provider-a", tx.ProviderID())
		assert.Equal(t, "ext-1", tx.ExternalTxID())
		assert.Empty(t, tx.IdempotencyKey(), "idempotency is attached by the application layer")
	})
	t.Run("internal kind is rejected over external channels", func(t *testing.T) {
		_, err := domain.NewExternalTransaction(domain.ExternalTxParams{
			ProviderID: "p", ExternalTransactionID: "e",
			PlayerID: domain.MustNewID(), WalletID: domain.MustNewID(),
			Kind: domain.KindOpening, Money: mustParse(t, "1.00", "BRL"),
			ReceivedVia: domain.ViaHTTP, Now: baseTime,
		})
		require.ErrorIs(t, err, domain.ErrInvalidKind)
		assert.False(t, domain.KindOpening.IsExternal())
	})
	t.Run("reversals require a reference", func(t *testing.T) {
		for _, kind := range []domain.TxKind{domain.KindRefund, domain.KindRollback} {
			_, err := domain.NewExternalTransaction(domain.ExternalTxParams{
				ProviderID: "p", ExternalTransactionID: "e",
				PlayerID: domain.MustNewID(), WalletID: domain.MustNewID(),
				Kind: kind, Money: mustParse(t, "1.00", "BRL"),
				ReceivedVia: domain.ViaSQS, Now: baseTime,
			})
			require.ErrorIs(t, err, domain.ErrMissingReference, "kind %s", kind)
		}
	})
	t.Run("round and game ids ride along", func(t *testing.T) {
		tx := externalTx(t, func(p *domain.ExternalTxParams) {
			p.RoundID, p.GameID = "round-9", "game-9"
		})
		assert.Equal(t, "round-9", tx.RoundID())
		assert.Equal(t, "game-9", tx.GameID())
	})
}

func TestNewOpeningTransaction(t *testing.T) {
	tx, err := domain.NewOpeningTransaction(
		domain.MustNewID(), domain.MustNewID(), "BRL", mustParse(t, "7.00", "BRL"), baseTime,
	)
	require.NoError(t, err)
	assert.Equal(t, domain.KindOpening, tx.Kind())
	assert.Equal(t, domain.OriginInternal, tx.Origin())
	assert.Equal(t, domain.StatusPending, tx.Status())
	assert.Equal(t, "wallet-opening", tx.CorrelationID())

	_, err = domain.NewOpeningTransaction(
		domain.MustNewID(), domain.MustNewID(), "BRL", mustParse(t, "-1.00", "BRL"), baseTime,
	)
	require.ErrorIs(t, err, domain.ErrInvalidAmount)
}

func TestTransactionStateMachine(t *testing.T) {
	t.Run("pending to processed with a financial result", func(t *testing.T) {
		tx := externalTx(t, nil)
		require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
			Direction:     domain.DirectionDebit,
			ResultAmount:  mustParse(t, "10.00", "BRL"),
			BalanceBefore: mustParse(t, "50.00", "BRL"),
			BalanceAfter:  mustParse(t, "40.00", "BRL"),
			WalletVersion: 2,
		}, baseTime))
		assert.Equal(t, domain.StatusProcessed, tx.Status())
		assert.True(t, tx.Status().Terminal())
		assert.True(t, tx.Terminal())
		require.NotNil(t, tx.BalanceAfter())
		assert.Equal(t, "40.00", tx.BalanceAfter().String())
		assert.Equal(t, int64(2), *tx.WalletVersionAfter())
	})
	t.Run("processed is terminal", func(t *testing.T) {
		tx := externalTx(t, nil)
		require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
			Direction:     domain.DirectionNone,
			ResultAmount:  mustParse(t, "0", "BRL"),
			BalanceBefore: domain.ZeroMoneyMust("BRL"),
			BalanceAfter:  domain.ZeroMoneyMust("BRL"),
			WalletVersion: 1,
		}, baseTime))
		err := tx.MarkRejected("LATE", "too late", baseTime)
		require.ErrorIs(t, err, domain.ErrInvalidTransition)
	})
	t.Run("pending reference retries then processes", func(t *testing.T) {
		tx := externalTx(t, func(p *domain.ExternalTxParams) {
			p.Kind = domain.KindRefund
			p.ReferenceExternalTxID = "ext-0"
		})
		retryAt := baseTime.Add(time.Minute)
		require.NoError(t, tx.MarkPendingReference(retryAt, baseTime))
		assert.Equal(t, domain.StatusPendingReference, tx.Status())
		assert.Equal(t, 1, tx.PendingAttempts())
		require.NotNil(t, tx.NextRetryAt())
		assert.WithinDuration(t, retryAt, *tx.NextRetryAt(), time.Second)

		// retries are allowed from PENDING_REFERENCE (self-transition)
		require.NoError(t, tx.MarkPendingReference(retryAt.Add(time.Minute), baseTime))
		assert.Equal(t, 2, tx.PendingAttempts())

		require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
			Direction:     domain.DirectionCredit,
			ResultAmount:  mustParse(t, "10.00", "BRL"),
			BalanceBefore: mustParse(t, "0.00", "BRL"),
			BalanceAfter:  mustParse(t, "10.00", "BRL"),
			WalletVersion: 3,
		}, baseTime))
		assert.Equal(t, domain.StatusProcessed, tx.Status())
	})
	t.Run("rejection and failure record the stable code", func(t *testing.T) {
		rejected := externalTx(t, nil)
		require.NoError(t, rejected.MarkRejected("INSUFFICIENT_FUNDS", "no funds", baseTime))
		assert.Equal(t, domain.StatusRejected, rejected.Status())
		assert.Equal(t, "INSUFFICIENT_FUNDS", rejected.FailureCode())

		failed := externalTx(t, nil)
		require.NoError(t, failed.MarkFailed("DB_ERROR", "boom", baseTime))
		assert.Equal(t, domain.StatusFailed, failed.Status())
	})
	t.Run("no-movement result must carry a zero amount", func(t *testing.T) {
		tx := externalTx(t, nil)
		err := tx.MarkProcessed(domain.ProcessedResult{
			Direction: domain.DirectionNone, ResultAmount: mustParse(t, "1.00", "BRL"),
		}, baseTime)
		require.Error(t, err)
	})
}

func TestSetIdempotency(t *testing.T) {
	tx := externalTx(t, nil)
	tx.SetIdempotency("idem-1", "hash-1")
	assert.Equal(t, "idem-1", tx.IdempotencyKey())
	assert.Equal(t, "hash-1", tx.PayloadHash())
}
