// Package domain_test exercises the ledger line construction and the error
// taxonomy, with testify.
package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func movement(t *testing.T, dir domain.Direction, amount, before, after string) domain.Movement {
	t.Helper()
	return domain.Movement{
		HasChange: true, Direction: dir,
		Amount:        mustParse(t, amount, "BRL"),
		BalanceBefore: mustParse(t, before, "BRL"),
		BalanceAfter:  mustParse(t, after, "BRL"),
		WalletVersion: 2,
	}
}

func TestNewLedgerEntry(t *testing.T) {
	t.Run("debit line balances", func(t *testing.T) {
		mv := movement(t, domain.DirectionDebit, "10.00", "50.00", "40.00")
		entry, err := domain.NewLedgerEntry(domain.MustNewID(), domain.MustNewID(), mv, baseTime)
		require.NoError(t, err)
		assert.Equal(t, domain.DirectionDebit, entry.Direction())
		assert.Equal(t, "10.00", entry.Amount().String())
		assert.Equal(t, "40.00", entry.BalanceAfter().String())
		assert.WithinDuration(t, baseTime, entry.CreatedAt(), time.Second)
	})
	t.Run("credit line balances", func(t *testing.T) {
		mv := movement(t, domain.DirectionCredit, "10.00", "40.00", "50.00")
		_, err := domain.NewLedgerEntry(domain.MustNewID(), domain.MustNewID(), mv, baseTime)
		require.NoError(t, err)
	})
	t.Run("arithmetic mismatch is a ledger error", func(t *testing.T) {
		mv := movement(t, domain.DirectionDebit, "10.00", "50.00", "41.00") // 50-10 != 41
		_, err := domain.NewLedgerEntry(domain.MustNewID(), domain.MustNewID(), mv, baseTime)
		require.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})
	t.Run("negative balance is a ledger error", func(t *testing.T) {
		mv := movement(t, domain.DirectionDebit, "10.00", "5.00", "-5.00")
		_, err := domain.NewLedgerEntry(domain.MustNewID(), domain.MustNewID(), mv, baseTime)
		require.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})
	t.Run("directionless movement is a ledger error", func(t *testing.T) {
		mv := movement(t, domain.DirectionNone, "10.00", "50.00", "40.00")
		_, err := domain.NewLedgerEntry(domain.MustNewID(), domain.MustNewID(), mv, baseTime)
		require.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})
	t.Run("rehydration accepts immutable committed state", func(t *testing.T) {
		entry, err := domain.RehydrateLedgerEntry(domain.LedgerEntryParams{
			ID: domain.MustNewID(), WalletID: domain.MustNewID(), TransactionID: domain.MustNewID(),
			Direction:     domain.DirectionCredit,
			Amount:        mustParse(t, "10.00", "BRL"),
			BalanceBefore: mustParse(t, "40.00", "BRL"),
			BalanceAfter:  mustParse(t, "50.00", "BRL"),
			CreatedAt:     baseTime,
		})
		require.NoError(t, err)
		assert.Equal(t, domain.DirectionCredit, entry.Direction())
	})
	t.Run("rehydration rejects an impossible direction", func(t *testing.T) {
		_, err := domain.RehydrateLedgerEntry(domain.LedgerEntryParams{
			ID: domain.MustNewID(), WalletID: domain.MustNewID(), TransactionID: domain.MustNewID(),
			Direction:     domain.DirectionNone,
			Amount:        mustParse(t, "10.00", "BRL"),
			BalanceBefore: mustParse(t, "40.00", "BRL"),
			BalanceAfter:  mustParse(t, "50.00", "BRL"),
			CreatedAt:     baseTime,
		})
		require.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})
}

func TestDomainErrorIdentity(t *testing.T) {
	t.Run("errors.Is holds through With", func(t *testing.T) {
		wrapped := domain.ErrInsufficientFunds.With("balance %s too low", "0.00")
		require.ErrorIs(t, wrapped, domain.ErrInsufficientFunds)
		assert.Contains(t, wrapped.Error(), "INSUFFICIENT_FUNDS")
		assert.Contains(t, wrapped.Error(), "too low")
	})
	t.Run("unknown errors classify as INTERNAL_ERROR", func(t *testing.T) {
		assert.Equal(t, "INTERNAL_ERROR", domain.FailureCode(errors.New("boom")))
	})
	t.Run("wrapped domain errors keep their code", func(t *testing.T) {
		assert.Equal(t, "CURRENCY_MISMATCH", domain.FailureCode(domain.ErrCurrencyMismatch.With("a vs b")))
	})
}

func TestParseID(t *testing.T) {
	id := domain.MustNewID()
	got, err := domain.ParseID(id.String())
	require.NoError(t, err)
	assert.Equal(t, id, got)

	_, err = domain.ParseID("not-a-uuid")
	require.Error(t, err)
	assert.Equal(t, "INVALID_TRANSACTION", domain.FailureCode(err))
}
