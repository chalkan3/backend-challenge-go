package app

import (
	"testing"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func mustMoney(t *testing.T, amount, currency string) domain.Money {
	t.Helper()
	m, err := domain.ParseMoney(amount, currency)
	if err != nil {
		t.Fatalf("parse money %s %s: %v", amount, currency, err)
	}
	return m
}

// TestCanonicalHashIdempotencyConflict covers the unit-level contract the
// challenge states for idempotency (§13, "conflito de payload para a mesma
// chave"): the hash is deterministic over business fields only, normalizes
// equivalent amount spellings, and changes whenever ANY business field
// changes — which is what makes same-key-different-content detectable as a
// conflict instead of a replay.
func TestCanonicalHashIdempotencyConflict(t *testing.T) {
	base := ExternalInput{
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-1",
		IdempotencyKey:        "provider-a:tx-1",
		PlayerID:              "player-1",
		WalletID:              "wallet-1",
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Amount:                "25.00",
		Currency:              "BRL",
	}
	baseHash, err := canonicalHash(base, mustMoney(t, "25.00", "BRL"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if len(baseHash) != 64 { // SHA-256 hex
		t.Fatalf("hash length: %d", len(baseHash))
	}

	// transport identity is excluded: same business content under a
	// different key/channel/correlation id must hash identically (this is
	// what lets HTTP and SQS share one idempotency space).
	transport := base
	transport.IdempotencyKey = "someone-elses-key"
	transport.ReceivedVia = domain.ViaSQS
	transport.CorrelationID = "corr-xyz"
	h, err := canonicalHash(transport, mustMoney(t, "25.00", "BRL"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h != baseHash {
		t.Fatalf("transport fields must not affect the hash: %s != %s", h, baseHash)
	}

	// equivalent amount spellings normalize to the same hash
	h, err = canonicalHash(base, mustMoney(t, "25.0", "BRL"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h != baseHash {
		t.Fatalf("amount normalization: %s != %s", h, baseHash)
	}

	// changing ANY business field must change the hash → the service treats
	// a reused key with this payload as a conflict (409), not a replay.
	type mutation struct {
		name   string
		apply  func(in *ExternalInput)
		amount func(in *ExternalInput) domain.Money
	}
	mutations := []mutation{
		{"providerId", func(in *ExternalInput) { in.ProviderID = "provider-b" }, nil},
		{"externalTransactionId", func(in *ExternalInput) { in.ExternalTransactionID = "tx-2" }, nil},
		{"playerId", func(in *ExternalInput) { in.PlayerID = "player-2" }, nil},
		{"walletId", func(in *ExternalInput) { in.WalletID = "wallet-2" }, nil},
		{"roundId", func(in *ExternalInput) { in.RoundID = "round-2" }, nil},
		{"gameId", func(in *ExternalInput) { in.GameID = "other-game" }, nil},
		{"kind", func(in *ExternalInput) { in.Kind = "WIN" }, nil},
		{"amount", func(in *ExternalInput) { in.Amount = "25.01" }, nil},
		{"currency", func(in *ExternalInput) { in.Currency = "USD" },
			func(in *ExternalInput) domain.Money { return mustMoney(t, in.Amount, in.Currency) }},
		{"referenceExternalTransactionId", func(in *ExternalInput) { in.ReferenceExternalTransactionID = "ref-9" }, nil},
	}
	for _, m := range mutations {
		in := base
		m.apply(&in)
		amount := mustMoney(t, in.Amount, in.Currency)
		if m.amount != nil {
			amount = m.amount(&in)
		}
		h, err := canonicalHash(in, amount)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		if h == baseHash {
			t.Fatalf("mutation %s produced the same hash — payload conflict would pass as a replay", m.name)
		}
	}
}
