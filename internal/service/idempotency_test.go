// Package service_test pins the canonical-hash contract used by durable
// idempotency (challenge §13): the hash covers business fields only,
// normalizes equivalent amount spellings and changes whenever ANY business
// field changes — which is what turns a same-key/different-payload retry
// into a conflict instead of a replay.
package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

func mustMoney(t *testing.T, amount, currency string) domain.Money {
	t.Helper()
	m, err := domain.ParseMoney(amount, currency)
	require.NoError(t, err)
	return m
}

func baseInput() service.ExternalInput {
	return service.ExternalInput{
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
}

func hashOf(t *testing.T, in service.ExternalInput) string {
	t.Helper()
	h, err := service.CanonicalHash(in, mustMoney(t, in.Amount, in.Currency))
	require.NoError(t, err)
	return h
}

func TestCanonicalHashNormalizesEquivalentPayloads(t *testing.T) {
	base := hashOf(t, baseInput())

	t.Run("amount spellings 25 / 25.0 / 25.00 are equivalent", func(t *testing.T) {
		for _, spelling := range []string{"25", "25.0", "25.00"} {
			in := baseInput()
			in.Amount = spelling
			assert.Equal(t, base, hashOf(t, in), "amount %q should hash like 25.00", spelling)
		}
	})

	t.Run("transport metadata is excluded", func(t *testing.T) {
		in := baseInput()
		in.IdempotencyKey = "another-key"
		in.CorrelationID = "corr-9"
		in.ReceivedVia = domain.ViaSQS
		assert.Equal(t, base, hashOf(t, in),
			"the same operation arriving on another channel must hash identically")
	})
}

func TestCanonicalHashDetectsPayloadMutations(t *testing.T) {
	baseHash := hashOf(t, baseInput())

	mutations := []struct {
		name   string
		amount string
		mutate func(*service.ExternalInput)
	}{
		{"providerId", "25.00", func(in *service.ExternalInput) { in.ProviderID = "provider-b" }},
		{"externalTransactionId", "25.00", func(in *service.ExternalInput) { in.ExternalTransactionID = "tx-2" }},
		{"playerId", "25.00", func(in *service.ExternalInput) { in.PlayerID = "player-2" }},
		{"walletId", "25.00", func(in *service.ExternalInput) { in.WalletID = "wallet-2" }},
		{"roundId", "25.00", func(in *service.ExternalInput) { in.RoundID = "round-2" }},
		{"gameId", "25.00", func(in *service.ExternalInput) { in.GameID = "other-game" }},
		{"kind", "25.00", func(in *service.ExternalInput) { in.Kind = "WIN" }},
		{"amount", "25.01", func(in *service.ExternalInput) {}},
		{"currency", "25.00", func(in *service.ExternalInput) { in.Currency = "USD" }},
		{"referenceExternalTransactionId", "25.00", func(in *service.ExternalInput) { in.ReferenceExternalTransactionID = "ref-9" }},
	}

	for _, tc := range mutations {
		t.Run(tc.name+"_mutation_changes_the_hash", func(t *testing.T) {
			in := baseInput()
			in.Amount = tc.amount
			tc.mutate(&in)
			assert.NotEqual(t, baseHash, hashOf(t, in),
				"a mutated payload must never pass as an idempotent replay")
		})
	}
}
