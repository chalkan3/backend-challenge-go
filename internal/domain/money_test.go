// Package domain_test exercises the Money value object through its exported
// API only (black-box), with table-driven cases and testify.
package domain_test

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func mustParse(t *testing.T, amount, currency string) domain.Money {
	t.Helper()
	m, err := domain.ParseMoney(amount, currency)
	require.NoError(t, err)
	return m
}

func requireMoney(t *testing.T, want string, got domain.Money) {
	t.Helper()
	require.Equal(t, want, got.String(), "money value mismatch")
}

func TestParseMoney(t *testing.T) {
	tests := []struct {
		name       string
		amount     string
		currency   string
		wantMinor  int64
		wantErrMsg string
	}{
		{name: "two decimals", amount: "1000.00", currency: "BRL", wantMinor: 100_000},
		{name: "one decimal", amount: "25.5", currency: "BRL", wantMinor: 2550},
		{name: "zero", amount: "0", currency: "BRL", wantMinor: 0},
		{name: "zero explicit", amount: "0.00", currency: "BRL", wantMinor: 0},
		{name: "negative", amount: "-5.25", currency: "BRL", wantMinor: -525},
		{name: "max representable", amount: "9999999999999999.99", currency: "BRL", wantMinor: 999_999_999_999_999_999},
		{name: "three fraction digits is rejected, never rounded", amount: "25.505", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "four fraction digits", amount: "25.5000", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "scientific notation", amount: "1e3", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "explicit plus sign", amount: "+25.00", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "empty amount", amount: "", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "blank amount", amount: "  ", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "not a number", amount: "abc", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "thousand separators", amount: "1,000.00", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "missing integer part", amount: ".50", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "17 integer digits overflows", amount: "10000000000000000.00", currency: "BRL", wantErrMsg: "INVALID_MONEY"},
		{name: "currency must be upper case", amount: "25.00", currency: "brl", wantErrMsg: "INVALID_CURRENCY"},
		{name: "currency must be alpha-3", amount: "25.00", currency: "BRLZ", wantErrMsg: "INVALID_CURRENCY"},
		{name: "currency is required", amount: "25.00", currency: "", wantErrMsg: "INVALID_CURRENCY"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := domain.ParseMoney(tc.amount, tc.currency)
			if tc.wantErrMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMinor, m.AmountMinor())
			assert.Equal(t, "BRL", m.Currency())
		})
	}
}

func TestMoneyStringIsExactDecimal(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{-1, "-0.01"},
		{2550, "25.50"},
		{-525, "-5.25"},
		{999_999_999_999_999_999, "9999999999999999.99"},
	}
	for _, tc := range tests {
		m, err := domain.NewMoney(tc.minor, "BRL")
		require.NoError(t, err)
		assert.Equal(t, tc.want, m.String())
	}
}

func TestMoneyArithmetic(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		got, err := mustParse(t, "10.10", "BRL").Add(mustParse(t, "20.20", "BRL"))
		require.NoError(t, err)
		requireMoney(t, "30.30", got)
	})
	t.Run("sub", func(t *testing.T) {
		got, err := mustParse(t, "30.00", "BRL").Sub(mustParse(t, "0.01", "BRL"))
		require.NoError(t, err)
		requireMoney(t, "29.99", got)
	})
	t.Run("neg", func(t *testing.T) {
		got, err := mustParse(t, "5.25", "BRL").Neg()
		require.NoError(t, err)
		requireMoney(t, "-5.25", got)
	})
	t.Run("currency mismatch is a domain error", func(t *testing.T) {
		_, err := mustParse(t, "1.00", "BRL").Add(mustParse(t, "1.00", "USD"))
		require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	})
	t.Run("add overflow is detected, never wrapped", func(t *testing.T) {
		a, err := domain.NewMoney(9_223_372_036_854_775_807, "BRL")
		require.NoError(t, err)
		one, err := domain.NewMoney(1, "BRL")
		require.NoError(t, err)
		_, err = a.Add(one)
		require.Error(t, err)
		assert.Contains(t, strings.ToUpper(err.Error()), "OVERFLOW")
	})
	t.Run("sub underflow is detected", func(t *testing.T) {
		min, err := domain.NewMoney(math.MinInt64, "BRL")
		require.NoError(t, err)
		one, err := domain.NewMoney(1, "BRL")
		require.NoError(t, err)
		_, err = min.Sub(one)
		require.Error(t, err)
	})
}

func TestMoneyPredicatesAndCompare(t *testing.T) {
	zero := mustParse(t, "0", "BRL")
	credit := mustParse(t, "1.00", "BRL")
	debit := mustParse(t, "-1.00", "BRL")

	assert.True(t, zero.IsZero())
	assert.True(t, credit.IsPositive())
	assert.True(t, debit.IsNegative())

	cmp, err := credit.Compare(debit)
	require.NoError(t, err)
	assert.Equal(t, 1, cmp)

	cmp, err = zero.Compare(mustParse(t, "0.00", "BRL"))
	require.NoError(t, err)
	assert.Equal(t, 0, cmp)

	_, err = credit.Compare(mustParse(t, "1.00", "USD"))
	require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
}

func TestZeroMoney(t *testing.T) {
	m, err := domain.ZeroMoney("BRL")
	require.NoError(t, err)
	requireMoney(t, "0.00", m)
}
