package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in     string
		cur    string
		want   int64
		wantEr bool
	}{
		{"1000.00", "BRL", 100000, false},
		{"25.5", "BRL", 2550, false},
		{"0", "BRL", 0, false},
		{"0.00", "BRL", 0, false},
		{"-5.25", "BRL", -525, false},
		{"9999999999999999.99", "BRL", 999999999999999999, false},
		{"25.505", "BRL", 0, true},  // 3 fraction digits: rejected, never rounded
		{"25.5000", "BRL", 0, true}, // 4 fraction digits
		{"1e3", "BRL", 0, true},
		{"+25.00", "BRL", 0, true},
		{"", "BRL", 0, true},
		{"  ", "BRL", 0, true},
		{"abc", "BRL", 0, true},
		{"1,000.00", "BRL", 0, true},
		{".50", "BRL", 0, true},
		{"10000000000000000.00", "BRL", 0, true}, // 17 integer digits: overflow guard
		{"25.00", "brl", 0, true},                // currency must be upper-case alpha-3
		{"25.00", "BRLZ", 0, true},
		{"25.00", "", 0, true},
	}
	for _, c := range cases {
		m, err := ParseMoney(c.in, c.cur)
		if c.wantEr {
			if err == nil {
				t.Errorf("ParseMoney(%q,%q): expected error", c.in, c.cur)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMoney(%q,%q): %v", c.in, c.cur, err)
			continue
		}
		if m.AmountMinor() != c.want {
			t.Errorf("ParseMoney(%q,%q) = %d, want %d", c.in, c.cur, m.AmountMinor(), c.want)
		}
	}
}

func TestMoneyStringRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "1000.00", "25.50", "-5.25", "9999999999999999.99"} {
		m := mustParse(t, s, "BRL")
		if got := m.String(); got != s {
			t.Errorf("round trip %q => %q", s, got)
		}
	}
	// canonicalization: "25.5" serializes as "25.50"
	if got := mustParse(t, "25.5", "BRL").String(); got != "25.50" {
		t.Errorf("canonicalize: %q", got)
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a := mustParse(t, "100.00", "BRL")
	b := mustParse(t, "80.00", "BRL")
	sum, err := a.Add(b)
	if err != nil || sum.AmountMinor() != 18000 {
		t.Fatalf("add: %v %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.AmountMinor() != 2000 {
		t.Fatalf("sub: %v %v", diff, err)
	}
	neg, err := b.Neg()
	if err != nil || neg.AmountMinor() != -8000 {
		t.Fatalf("neg: %v %v", neg, err)
	}
	if c, _ := a.Compare(b); c != 1 {
		t.Fatal("compare a>b")
	}
	if c, _ := b.Compare(b); c != 0 {
		t.Fatal("compare b==b")
	}
	// currency mismatch is classifiable via errors.Is
	usd := mustParse(t, "1.00", "USD")
	if _, err := a.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("add cross-currency: %v", err)
	}
	if _, err := a.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("sub cross-currency: %v", err)
	}
	if _, err := a.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("compare cross-currency: %v", err)
	}
	// int64 overflow guards
	huge := Money{amountMinor: math.MaxInt64, currency: "BRL"}
	if _, err := huge.Add(mustParse(t, "0.01", "BRL")); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("add overflow: %v", err)
	}
	minv := Money{amountMinor: math.MinInt64, currency: "BRL"}
	if _, err := minv.Sub(mustParse(t, "0.01", "BRL")); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("sub overflow: %v", err)
	}
	if _, err := minv.Neg(); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("neg MinInt64: %v", err)
	}
	// zero per currency
	z, err := ZeroMoney("BRL")
	if err != nil || !z.IsZero() || z.String() != "0.00" {
		t.Fatalf("zero: %v %v", z, err)
	}
}

func TestOpenWallet(t *testing.T) {
	now := time.Now().UTC()
	init := mustParse(t, "1000.00", "BRL")
	w, mv, err := OpenWallet(newIDMust(t), "BRL", init, now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || w.Balance().AmountMinor() != 100000 {
		t.Fatalf("wallet v=%d balance=%s", w.Version(), w.Balance().String())
	}
	if mv.Direction != DirectionCredit || mv.Amount.AmountMinor() != 100000 || mv.WalletVersion != 1 {
		t.Fatalf("movement %+v", mv)
	}
	if !mv.BalanceAfter.Equals(mustParse(t, "1000.00", "BRL")) {
		t.Fatal("balanceAfter")
	}

	// zero opening: no movement, version stays 1
	w0, mv0, err := OpenWallet(newIDMust(t), "BRL", mustParse(t, "0", "BRL"), now)
	if err != nil || mv0.Direction != "" {
		t.Fatalf("zero opening: %v %+v", err, mv0)
	}
	if w0.Version() != 1 {
		t.Fatal("zero opening must not bump version")
	}

	// negative or currency mismatch rejected
	if _, _, err := OpenWallet(newIDMust(t), "BRL", mustParse(t, "-1.00", "BRL"), now); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("negative opening: %v", err)
	}
	if _, _, err := OpenWallet(newIDMust(t), "BRL", mustParse(t, "1.00", "USD"), now); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("cross currency opening: %v", err)
	}
}

func TestWalletDebitCreditInvariants(t *testing.T) {
	now := time.Now().UTC()
	w, _, _ := OpenWallet(newIDMust(t), "BRL", mustParse(t, "100.00", "BRL"), now)

	if _, err := w.Credit(mustParse(t, "1.00", "USD"), now); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("credit cross currency: %v", err)
	}
	if _, err := w.Credit(mustParse(t, "0.00", "BRL"), now); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("credit zero: %v", err)
	}
	if _, err := w.Debit(mustParse(t, "0.00", "BRL"), now); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("debit zero: %v", err)
	}
	vBefore := w.Version()
	if _, err := w.Debit(mustParse(t, "100.01", "BRL"), now); !errors.Is(err, ErrInsufficientFunds) {
		t.Errorf("over debit: %v", err)
	}
	if w.Version() != vBefore {
		t.Error("failed debit must not bump version")
	}
	mv, err := w.Debit(mustParse(t, "100.00", "BRL"), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().AmountMinor() != 0 || mv.WalletVersion != 2 || mv.BalanceAfter.IsNegative() {
		t.Fatalf("debit to zero: %+v balance=%s", mv, w.Balance().String())
	}
	if _, err := RehydrateWallet(newIDMust(t), newIDMust(t), "BRL", -100, 1, now, now); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("rehydrate negative: %v", err)
	}
	// version bumps only on real balance changes
	w2, _, _ := OpenWallet(newIDMust(t), "BRL", mustParse(t, "50.00", "BRL"), now)
	_, err = w2.Credit(mustParse(t, "25.00", "BRL"), now)
	if err != nil || w2.Version() != 2 {
		t.Fatalf("credit version: %d %v", w2.Version(), err)
	}
	_, err = w2.Debit(mustParse(t, "75.01", "BRL"), now)
	if !errors.Is(err, ErrInsufficientFunds) || w2.Version() != 2 {
		t.Fatalf("failed debit version: %d %v", w2.Version(), err)
	}
}

func TestLedgerEntryArithmetic(t *testing.T) {
	now := time.Now().UTC()
	before := mustParse(t, "100.00", "BRL")
	amount := mustParse(t, "30.00", "BRL")
	after := mustParse(t, "70.00", "BRL")
	if _, err := NewLedgerEntry(newIDMust(t), newIDMust(t), Movement{HasChange: true, Direction: DirectionDebit, Amount: amount, BalanceBefore: before, BalanceAfter: after, WalletVersion: 2}, now); err != nil {
		t.Fatalf("valid debit entry: %v", err)
	}
	if _, err := NewLedgerEntry(newIDMust(t), newIDMust(t), Movement{HasChange: true, Direction: DirectionDebit, Amount: amount, BalanceBefore: before, BalanceAfter: mustParse(t, "69.99", "BRL"), WalletVersion: 2}, now); !errors.Is(err, ErrLedgerArithmetic) {
		t.Errorf("tampered entry: %v", err)
	}
	if _, err := NewLedgerEntry(newIDMust(t), newIDMust(t), Movement{Direction: DirectionCredit, Amount: mustParse(t, "0.00", "BRL"), BalanceBefore: before, BalanceAfter: before, WalletVersion: 2}, now); !errors.Is(err, ErrLedgerArithmetic) {
		t.Errorf("zero amount entry: %v", err)
	}
	// rehydrated credit entry validates too
	if _, err := RehydrateLedgerEntry(LedgerEntryParams{ID: newIDMust(t), WalletID: newIDMust(t), TransactionID: newIDMust(t), Direction: DirectionCredit, Amount: amount, BalanceBefore: before, BalanceAfter: mustParse(t, "130.00", "BRL"), CreatedAt: now}); err != nil {
		t.Errorf("valid credit entry: %v", err)
	}
}

func TestFailureCodeClassification(t *testing.T) {
	if got := FailureCode(ErrInsufficientFunds); got != "INSUFFICIENT_FUNDS" {
		t.Errorf("sentinel: %s", got)
	}
	wrapped := ErrCurrencyMismatch.With("x vs y")
	if got := FailureCode(wrapped); got != "CURRENCY_MISMATCH" {
		t.Errorf("wrapped: %s", got)
	}
	if got := FailureCode(errors.New("boom")); got != "INTERNAL_ERROR" {
		t.Errorf("unknown: %s", got)
	}
}
