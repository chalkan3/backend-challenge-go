package domain

import (
	"testing"

	"github.com/google/uuid"
)

// Shared test helpers for the domain package.

func NewIDMustT() uuid.UUID {
	u, err := NewID()
	if err != nil {
		panic(err)
	}
	return u
}

func newIDMust(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustParse(t *testing.T, s, cur string) Money {
	t.Helper()
	m, err := ParseMoney(s, cur)
	if err != nil {
		t.Fatalf("ParseMoney(%q,%q): %v", s, cur, err)
	}
	return m
}

func (m Money) Equals(o Money) bool {
	return m.amountMinor == o.amountMinor && m.currency == o.currency
}

// p returns a pointer to m (test helper).
func (m Money) p() *Money { return &m }
