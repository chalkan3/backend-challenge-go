// The remaining guard branches of the money and wallet value objects: the
// integer-overflow refusals and the currency-shape checks. They are the
// difference between a system that fails closed on a nonsensical amount and
// one that wraps around to a plausible-looking balance, so each is asserted to
// fire on the input that triggers it — and to leave the aggregate unchanged.
package domain_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func TestMoneyOverflowGuards(t *testing.T) {
	t.Run("Sub refuses a currency mismatch", func(t *testing.T) {
		_, err := mustParse(t, "1.00", "BRL").Sub(mustParse(t, "1.00", "USD"))
		require.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	})

	t.Run("Sub detects positive underflow", func(t *testing.T) {
		// max - (-1) wraps past MaxInt64: the sign of the operands rules the
		// guard, not the sign of the result.
		max, err := domain.NewMoney(math.MaxInt64, "BRL")
		require.NoError(t, err)
		negOne, err := domain.NewMoney(-1, "BRL")
		require.NoError(t, err)
		_, err = max.Sub(negOne)
		require.ErrorIs(t, err, domain.ErrMoneyOverflow)
	})

	t.Run("Neg refuses MinInt64", func(t *testing.T) {
		// -MinInt64 is not representable, so negation must be refused rather
		// than silently yielding the negative of the same value.
		min, err := domain.NewMoney(math.MinInt64, "BRL")
		require.NoError(t, err)
		_, err = min.Neg()
		require.ErrorIs(t, err, domain.ErrMoneyOverflow)
	})
}

func TestParseMoneyRejectsOversizedInput(t *testing.T) {
	t.Run("more integer digits than the shape allows", func(t *testing.T) {
		// The decimal shape caps the integer part at 16 digits, so anything
		// longer is rejected before any arithmetic happens.
		_, err := domain.ParseMoney("99999999999999999.00", "BRL")
		require.ErrorIs(t, err, domain.ErrInvalidMoney)
	})

	t.Run("fractional part beyond the scale is rejected", func(t *testing.T) {
		_, err := domain.ParseMoney("1.999", "BRL")
		require.ErrorIs(t, err, domain.ErrInvalidMoney,
			"more than two decimals is not a valid amount")
	})

	t.Run("a bad currency code is rejected before the amount", func(t *testing.T) {
		_, err := domain.ParseMoney("1.00", "BR")
		require.Error(t, err)
	})

	t.Run("a leading sign without an integer part is rejected", func(t *testing.T) {
		// "-.50" is not the documented shape; the amount must carry a digit.
		_, err := domain.ParseMoney("-.50", "BRL")
		require.ErrorIs(t, err, domain.ErrInvalidMoney)
	})
}

func TestRehydrateWalletRejectsBadInputs(t *testing.T) {
	t.Run("invalid currency", func(t *testing.T) {
		_, err := domain.RehydrateWallet(
			domain.MustNewID(), domain.MustNewID(), "BR", 100, 1, baseTime, baseTime,
		)
		require.Error(t, err)
	})
}

func TestWalletApplyRefusesOverflow(t *testing.T) {
	// A wallet already at the maximum cannot take one more minor unit: the
	// credit must fail and leave both balance and version untouched, otherwise
	// version would advance past a movement that never happened.
	w, err := domain.RehydrateWallet(
		domain.MustNewID(), domain.MustNewID(), "BRL", math.MaxInt64, 7, baseTime, baseTime,
	)
	require.NoError(t, err)

	one, err := domain.NewMoney(1, "BRL")
	require.NoError(t, err)

	mv, err := w.Credit(one, baseTime)
	require.ErrorIs(t, err, domain.ErrMoneyOverflow)
	assert.False(t, mv.HasChange)
	assert.Equal(t, int64(7), w.Version(), "a failed movement must not bump the version")
	assert.Equal(t, int64(math.MaxInt64), w.Balance().AmountMinor())
}

func TestOwnershipGuard(t *testing.T) {
	// ZeroMoneyMust is called where the currency was already validated: it
	// exists to turn a programmer error into a loud panic, never a quiet zero.
	assert.Panics(t, func() { domain.ZeroMoneyMust("BR") },
		"an invalid currency is a programmer error, so it must panic")
	require.NotPanics(t, func() { domain.ZeroMoneyMust("BRL") })
}
