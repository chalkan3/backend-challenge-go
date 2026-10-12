package domain

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Money is an immutable value object: an int64 amount in minor units
// (e.g. cents) plus an ISO 4217 currency code. No float32/float64 is used
// anywhere in parsing, arithmetic or serialization.
//
// Representation and limits (documented in ARCHITECTURE.md):
//   - scale is fixed at MoneyScale = 2 decimal digits for every currency
//     (the challenge allows BRL-only scenarios as long as the currency is
//     carried; amounts with more precision are rejected, never rounded).
//   - range: ±9,999,999,999,999,999.99 (16 integer digits), comfortably
//     inside int64. Overflow is detected and reported, never wrapped.
type Money struct {
	amountMinor int64
	currency    string
}

const (
	MoneyScale        = 2
	minorUnitsPerUnit = int64(100)
	maxIntegerDigits  = 16
)

var decimalPattern = regexp.MustCompile(`^-?[0-9]{1,16}(\.[0-9]{1,2})?$`)

// NewMoney builds a Money from minor units and a currency code.
func NewMoney(amountMinor int64, currency string) (Money, error) {
	if err := ValidateCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{amountMinor: amountMinor, currency: currency}, nil
}

// ZeroMoney returns the zero amount for a currency.
func ZeroMoney(currency string) (Money, error) { return NewMoney(0, currency) }

// ParseMoney parses a decimal string such as "1000.00" into minor units
// without ever touching floating point. Accepted forms: "-?" 1..16 digits,
// optionally followed by "." plus 1..2 digits. "25" is accepted as 25.00.
// Values with more than 2 decimals are rejected (no silent precision loss).
func ParseMoney(amount, currency string) (Money, error) {
	if err := ValidateCurrency(currency); err != nil {
		return Money{}, err
	}
	s := strings.TrimSpace(amount)
	if s == "" || !decimalPattern.MatchString(s) {
		return Money{}, ErrInvalidMoney.With("amount %q is not a valid decimal value", amount)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	for len(fracPart) < MoneyScale {
		fracPart += "0"
	}
	if len(intPart) > maxIntegerDigits {
		return Money{}, ErrMoneyOverflow.With("integer part of %q exceeds %d digits", amount, maxIntegerDigits)
	}
	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Money{}, ErrMoneyOverflow
	}
	minor, err := strconv.ParseInt(fracPart, 10, 32)
	if err != nil {
		return Money{}, ErrMoneyOverflow
	}
	total := units*minorUnitsPerUnit + minor
	if neg {
		total = -total
	}
	return Money{amountMinor: total, currency: currency}, nil
}

// ValidateCurrency checks the ISO 4217 alpha-3 shape of a currency code.
func ValidateCurrency(c string) error {
	if len(c) != 3 {
		return ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return ErrInvalidCurrency
		}
	}
	return nil
}

// Currency returns the ISO 4217 code.
func (m Money) Currency() string { return m.currency }

// AmountMinor returns the amount in minor units (exact, no floats).
func (m Money) AmountMinor() int64 { return m.amountMinor }

func (m Money) IsZero() bool     { return m.amountMinor == 0 }
func (m Money) IsPositive() bool { return m.amountMinor > 0 }
func (m Money) IsNegative() bool { return m.amountMinor < 0 }

// Add sums two amounts of the same currency.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch.With("%s vs %s", m.currency, o.currency)
	}
	sum := m.amountMinor + o.amountMinor
	if (m.amountMinor > 0 && o.amountMinor > 0 && sum < 0) ||
		(m.amountMinor < 0 && o.amountMinor < 0 && sum >= 0) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{amountMinor: sum, currency: m.currency}, nil
}

// Sub subtracts o from m (same currency).
func (m Money) Sub(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch.With("%s vs %s", m.currency, o.currency)
	}
	diff := m.amountMinor - o.amountMinor
	if (o.amountMinor > 0 && diff > m.amountMinor) ||
		(o.amountMinor < 0 && diff < m.amountMinor) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{amountMinor: diff, currency: m.currency}, nil
}

// Neg returns -m.
func (m Money) Neg() (Money, error) {
	if m.amountMinor == math.MinInt64 {
		return Money{}, ErrMoneyOverflow
	}
	return Money{amountMinor: -m.amountMinor, currency: m.currency}, nil
}

// Compare returns -1, 0 or 1. Errors when currencies differ.
func (m Money) Compare(o Money) (int, error) {
	if m.currency != o.currency {
		return 0, ErrCurrencyMismatch.With("%s vs %s", m.currency, o.currency)
	}
	switch {
	case m.amountMinor < o.amountMinor:
		return -1, nil
	case m.amountMinor > o.amountMinor:
		return 1, nil
	default:
		return 0, nil
	}
}

// String renders the exact decimal representation, always with MoneyScale
// decimals (e.g. "-42.50"). Safe for math.MinInt64.
func (m Money) String() string {
	mag := uint64(m.amountMinor)
	sign := ""
	if m.amountMinor < 0 {
		sign = "-"
		mag = uint64(-m.amountMinor)
	}
	units := mag / uint64(minorUnitsPerUnit)
	frac := mag % uint64(minorUnitsPerUnit)
	return fmt.Sprintf("%s%d.%02d", sign, units, frac)
}
