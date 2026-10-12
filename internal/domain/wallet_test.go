// Package domain_test exercises the Wallet aggregate through its exported
// API, with testify assertions.
package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

var baseTime = time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

func openWallet(t *testing.T, initial string) (*domain.Wallet, domain.Movement) {
	t.Helper()
	w, mv, err := domain.OpenWallet(domain.MustNewID(), "BRL", mustParse(t, initial, "BRL"), baseTime)
	require.NoError(t, err)
	return w, mv
}

func TestOpenWallet(t *testing.T) {
	t.Run("positive opening produces a credit movement", func(t *testing.T) {
		w, mv := openWallet(t, "100.00")
		assert.True(t, mv.HasChange)
		assert.Equal(t, domain.DirectionCredit, mv.Direction)
		assert.Equal(t, "0.00", mv.BalanceBefore.String())
		assert.Equal(t, "100.00", mv.BalanceAfter.String())
		assert.Equal(t, int64(1), w.Version(), "the opening itself is version 1")
		assert.Equal(t, "100.00", w.Balance().String())
	})
	t.Run("zero opening creates the wallet without movement", func(t *testing.T) {
		w, mv := openWallet(t, "0")
		assert.False(t, mv.HasChange)
		assert.Equal(t, int64(1), w.Version())
		assert.True(t, w.Balance().IsZero())
	})
	t.Run("rejects negative opening", func(t *testing.T) {
		_, _, err := domain.OpenWallet(domain.MustNewID(), "BRL", mustParse(t, "-0.01", "BRL"), baseTime)
		require.ErrorIs(t, err, domain.ErrInvalidAmount)
	})
	t.Run("rejects currency mismatch", func(t *testing.T) {
		_, _, err := domain.OpenWallet(domain.MustNewID(), "USD", mustParse(t, "1.00", "BRL"), baseTime)
		require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	})
	t.Run("rejects invalid currency", func(t *testing.T) {
		_, _, err := domain.OpenWallet(domain.MustNewID(), "BRLZ", domain.ZeroMoneyMust("BRL"), baseTime)
		require.Error(t, err)
	})
}

func TestWalletDebitCreditInvariants(t *testing.T) {
	w, _ := openWallet(t, "50.00")

	t.Run("debit produces a debit movement and bumps the version", func(t *testing.T) {
		mv, err := w.Debit(mustParse(t, "20.00", "BRL"), baseTime.Add(time.Minute))
		require.NoError(t, err)
		assert.True(t, mv.HasChange)
		assert.Equal(t, domain.DirectionDebit, mv.Direction)
		assert.Equal(t, "50.00", mv.BalanceBefore.String())
		assert.Equal(t, "30.00", mv.BalanceAfter.String())
		assert.Equal(t, int64(2), mv.WalletVersion)
		assert.Equal(t, "30.00", w.Balance().String())
	})
	t.Run("insufficient funds never mutates the balance", func(t *testing.T) {
		mv, err := w.Debit(mustParse(t, "30.01", "BRL"), baseTime.Add(2*time.Minute))
		require.ErrorIs(t, err, domain.ErrInsufficientFunds)
		assert.False(t, mv.HasChange)
		assert.Equal(t, "30.00", w.Balance().String(), "balance unchanged after a failed debit")
		assert.Equal(t, int64(2), w.Version(), "version unchanged after a failed debit")
	})
	t.Run("credit accumulates", func(t *testing.T) {
		mv, err := w.Credit(mustParse(t, "0.01", "BRL"), baseTime.Add(3*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, domain.DirectionCredit, mv.Direction)
		assert.Equal(t, "30.01", w.Balance().String())
	})
	t.Run("currency mismatch is rejected", func(t *testing.T) {
		_, err := w.Credit(mustParse(t, "1.00", "USD"), baseTime)
		require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	})
	t.Run("debiting the exact balance reaches zero and stays valid", func(t *testing.T) {
		mv, err := w.Debit(mustParse(t, "30.01", "BRL"), baseTime.Add(4*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, "0.00", mv.BalanceAfter.String())
		assert.True(t, w.Balance().IsZero())
	})
}

func TestRehydrateWallet(t *testing.T) {
	w, _ := openWallet(t, "42.00")

	got, err := domain.RehydrateWallet(
		w.ID(), w.PlayerID(), w.Currency(),
		w.Balance().AmountMinor(), w.Version(), w.CreatedAt(), w.UpdatedAt(),
	)
	require.NoError(t, err)
	assert.Equal(t, w.ID(), got.ID())
	assert.Equal(t, w.PlayerID(), got.PlayerID())
	assert.Equal(t, "42.00", got.Balance().String())
	assert.Equal(t, w.Version(), got.Version())

	t.Run("rejects negative balance", func(t *testing.T) {
		_, err := domain.RehydrateWallet(
			domain.MustNewID(), domain.MustNewID(), "BRL", -1, 1, baseTime, baseTime,
		)
		require.Error(t, err)
	})
	t.Run("rejects version zero", func(t *testing.T) {
		_, err := domain.RehydrateWallet(
			domain.MustNewID(), domain.MustNewID(), "BRL", 0, 0, baseTime, baseTime,
		)
		require.ErrorIs(t, err, domain.ErrZeroVersion)
	})
}
