// Package domain_test exercises the rehydration paths (rebuilding aggregates
// from their persisted representation) and the pending-reference event.
//
// These paths are pure: no database, no clock, no network. They were the
// largest uncovered surface of the domain package, and they matter because
// every read from Postgres goes through them — a bug here silently changes
// what the service believes it persisted.
package domain_test

import (
	"encoding/json"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// ── RehydrateWagerTransaction ──────────────────────────────────────────────

func strPtr(s string) *string { return &s }

func rehydrateParams(t *testing.T) domain.RehydrateParams {
	t.Helper()
	ext := "ext-42"
	key := "idem-42"
	hash := "hash-42"
	round := "round-7"
	game := "game-9"
	ref := "ext-ref"
	before := int64(100_00)
	after := int64(90_00)
	version := int64(4)
	return domain.RehydrateParams{
		ID:                    domain.MustNewID(),
		Kind:                  domain.KindBet,
		Origin:                domain.OriginExternal,
		ProviderID:            strPtr("provider-a"),
		ExternalTxID:          &ext,
		IdempotencyKey:        &key,
		PayloadHash:           &hash,
		PlayerID:              domain.MustNewID(),
		WalletID:              domain.MustNewID(),
		RoundID:               &round,
		GameID:                &game,
		Currency:              "BRL",
		AmountMinor:           10_00,
		ReferenceExternalTxID: &ref,
		Status:                domain.StatusProcessed,
		ResultDirection:       domain.DirectionDebit,
		ResultAmountMinor:     10_00,
		BalanceBeforeMinor:    &before,
		BalanceAfterMinor:     &after,
		WalletVersionAfter:    &version,
		CorrelationID:         "corr-42",
		ReceivedVia:           domain.ViaHTTP,
		PendingAttempts:       2,
		CreatedAt:             baseTime,
		UpdatedAt:             baseTime.Add(time.Second),
	}
}

func TestRehydrateWagerTransaction(t *testing.T) {
	t.Run("rebuilds every field from the persisted row", func(t *testing.T) {
		p := rehydrateParams(t)
		tx, err := domain.RehydrateWagerTransaction(p)
		require.NoError(t, err)

		assert.Equal(t, p.ID, tx.ID())
		assert.Equal(t, domain.KindBet, tx.Kind())
		assert.Equal(t, domain.OriginExternal, tx.Origin())
		assert.Equal(t, domain.StatusProcessed, tx.Status())
		assert.Equal(t, "provider-a", tx.ProviderID())
		assert.Equal(t, "ext-42", tx.ExternalTxID())
		assert.Equal(t, "idem-42", tx.IdempotencyKey())
		assert.Equal(t, "hash-42", tx.PayloadHash())
		assert.Equal(t, p.PlayerID, tx.PlayerID())
		assert.Equal(t, p.WalletID, tx.WalletID())
		assert.Equal(t, "round-7", tx.RoundID())
		assert.Equal(t, "game-9", tx.GameID())
		assert.Equal(t, "corr-42", tx.CorrelationID())
		assert.Equal(t, domain.ViaHTTP, tx.ReceivedVia())
		assert.Equal(t, 2, tx.PendingAttempts())
		assert.Equal(t, baseTime, tx.CreatedAt())
		assert.Equal(t, baseTime.Add(time.Second), tx.UpdatedAt())

		// Money is rebuilt from minor units, never from a float.
		assert.Equal(t, "10.00", tx.Amount().String())
		assert.Equal(t, "BRL", tx.Amount().Currency())
	})

	t.Run("restores the financial result of a PROCESSED row", func(t *testing.T) {
		tx, err := domain.RehydrateWagerTransaction(rehydrateParams(t))
		require.NoError(t, err)

		require.NotNil(t, tx.BalanceBefore())
		assert.Equal(t, domain.DirectionDebit, tx.ResultDirection())
		assert.Equal(t, "10.00", tx.ResultAmount().String())
		assert.Equal(t, "100.00", tx.BalanceBefore().String())
		require.NotNil(t, tx.BalanceAfter())
		assert.Equal(t, "90.00", tx.BalanceAfter().String())
		require.NotNil(t, tx.WalletVersionAfter)
		assert.Equal(t, int64(4), *tx.WalletVersionAfter())
	})

	t.Run("a PENDING row has no balance snapshot", func(t *testing.T) {
		p := rehydrateParams(t)
		p.Status = domain.StatusPending
		p.ResultDirection = ""
		p.ResultAmountMinor = 0
		p.BalanceBeforeMinor = nil
		p.BalanceAfterMinor = nil
		p.WalletVersionAfter = nil

		tx, err := domain.RehydrateWagerTransaction(p)
		require.NoError(t, err)
		// A row that moved no money carries an empty direction and a zero amount.
		assert.Equal(t, domain.Direction(""), tx.ResultDirection())
		assert.True(t, tx.ResultAmount().IsZero())
		assert.Nil(t, tx.BalanceBefore())
		assert.Nil(t, tx.BalanceAfter())
		assert.Nil(t, tx.WalletVersionAfter())
	})

	t.Run("a PENDING_REFERENCE row keeps its retry schedule", func(t *testing.T) {
		next := baseTime.Add(time.Minute)
		p := rehydrateParams(t)
		p.Status = domain.StatusPendingReference
		p.ResultDirection = ""
		p.ResultAmountMinor = 0
		p.BalanceBeforeMinor = nil
		p.BalanceAfterMinor = nil
		p.WalletVersionAfter = nil
		p.NextRetryAt = &next

		tx, err := domain.RehydrateWagerTransaction(p)
		require.NoError(t, err)
		require.NotNil(t, tx.NextRetryAt())
		assert.Equal(t, next, *tx.NextRetryAt())
	})

	t.Run("an OPENING row carries no provider identity", func(t *testing.T) {
		p := rehydrateParams(t)
		p.Kind = domain.KindOpening
		p.Origin = domain.OriginInternal
		p.ProviderID = nil
		p.ExternalTxID = nil
		p.IdempotencyKey = nil
		p.PayloadHash = nil
		p.ReferenceExternalTxID = nil

		tx, err := domain.RehydrateWagerTransaction(p)
		require.NoError(t, err)
		assert.Equal(t, domain.KindOpening, tx.Kind())
		assert.Equal(t, domain.OriginInternal, tx.Origin())
		assert.Empty(t, tx.ProviderID())
		assert.Empty(t, tx.ExternalTxID())
		assert.Empty(t, tx.IdempotencyKey())
	})

	t.Run("rejects a persisted row with a non-canonical currency", func(t *testing.T) {
		p := rehydrateParams(t)
		p.Currency = "brl" // lowercase is not the canonical form
		_, err := domain.RehydrateWagerTransaction(p)
		require.Error(t, err)
	})
}

// ── RetryPendingReference ──────────────────────────────────────────────────
// This is the entry point the reference worker calls on every redelivery.

func pendingReferenceTx(t *testing.T) *domain.WagerTransaction {
	t.Helper()
	tx := externalTx(t, func(p *domain.ExternalTxParams) {
		p.Kind = domain.KindRefund
		p.ReferenceExternalTxID = "ext-ref"
	})
	require.NoError(t, tx.MarkPendingReference(baseTime.Add(time.Minute), baseTime))
	return tx
}

func TestRetryPendingReference(t *testing.T) {
	t.Run("increments the attempt counter and reschedules", func(t *testing.T) {
		tx := pendingReferenceTx(t)
		assert.Equal(t, 1, tx.PendingAttempts())

		next := baseTime.Add(5 * time.Minute)
		require.NoError(t, tx.RetryPendingReference(next))

		assert.Equal(t, 2, tx.PendingAttempts())
		require.NotNil(t, tx.NextRetryAt())
		assert.Equal(t, next, *tx.NextRetryAt())
		// Retrying must not move the state machine.
		assert.Equal(t, domain.StatusPendingReference, tx.Status())
	})

	t.Run("each retry advances the counter monotonically", func(t *testing.T) {
		tx := pendingReferenceTx(t)
		for i := 2; i <= 5; i++ {
			require.NoError(t, tx.RetryPendingReference(baseTime.Add(time.Duration(i)*time.Minute)))
			assert.Equal(t, i, tx.PendingAttempts())
		}
	})

	t.Run("refuses to retry a transaction that is not PENDING_REFERENCE", func(t *testing.T) {
		tx := externalTx(t, nil) // PENDING
		err := tx.RetryPendingReference(baseTime.Add(time.Minute))
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrInvalidTransition)
	})

	t.Run("a PROCESSED transaction cannot be retried", func(t *testing.T) {
		tx := externalTx(t, nil)
		require.NoError(t, tx.MarkProcessed(domain.ProcessedResult{
			Direction:     domain.DirectionDebit,
			ResultAmount:  mustParse(t, "10.00", "BRL"),
			BalanceBefore: mustParse(t, "100.00", "BRL"),
			BalanceAfter:  mustParse(t, "90.00", "BRL"),
			WalletVersion: 2,
		}, baseTime))
		err := tx.RetryPendingReference(baseTime.Add(time.Minute))
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrInvalidTransition)
	})

	t.Run("a REJECTED transaction cannot be retried", func(t *testing.T) {
		tx := externalTx(t, nil)
		require.NoError(t, tx.MarkRejected("INSUFFICIENT_FUNDS", "no funds", baseTime))
		err := tx.RetryPendingReference(baseTime.Add(time.Minute))
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrInvalidTransition)
	})
}

// ── RehydrateLedgerEntry ───────────────────────────────────────────────────
// The domain documents this as a deliberate non-validating read path: a
// committed ledger line is immutable, so only its shape is checked.

func ledgerParams(t *testing.T) domain.LedgerEntryParams {
	t.Helper()
	return domain.LedgerEntryParams{
		ID:            domain.MustNewID(),
		WalletID:      domain.MustNewID(),
		TransactionID: domain.MustNewID(),
		Direction:     domain.DirectionDebit,
		Amount:        mustParse(t, "10.00", "BRL"),
		BalanceBefore: mustParse(t, "100.00", "BRL"),
		BalanceAfter:  mustParse(t, "90.00", "BRL"),
		CreatedAt:     baseTime,
	}
}

func TestRehydrateLedgerEntry(t *testing.T) {
	t.Run("rebuilds a debit line", func(t *testing.T) {
		p := ledgerParams(t)
		e, err := domain.RehydrateLedgerEntry(p)
		require.NoError(t, err)
		assert.Equal(t, p.ID, e.ID())
		assert.Equal(t, p.WalletID, e.WalletID())
		assert.Equal(t, p.TransactionID, e.TransactionID())
		assert.Equal(t, domain.DirectionDebit, e.Direction())
		assert.Equal(t, "BRL", e.Currency())
		assert.Equal(t, "10.00", e.Amount().String())
		assert.Equal(t, "100.00", e.BalanceBefore().String())
		assert.Equal(t, "90.00", e.BalanceAfter().String())
		assert.Equal(t, baseTime, e.CreatedAt())
	})

	t.Run("rebuilds a credit line", func(t *testing.T) {
		p := ledgerParams(t)
		p.Direction = domain.DirectionCredit
		p.BalanceBefore = mustParse(t, "90.00", "BRL")
		p.BalanceAfter = mustParse(t, "115.00", "BRL")
		p.Amount = mustParse(t, "25.00", "BRL")

		e, err := domain.RehydrateLedgerEntry(p)
		require.NoError(t, err)
		assert.Equal(t, domain.DirectionCredit, e.Direction())
		assert.Equal(t, "115.00", e.BalanceAfter().String())
	})

	t.Run("rejects a persisted line with an unknown direction", func(t *testing.T) {
		p := ledgerParams(t)
		p.Direction = domain.Direction("SIDEWAYS")
		_, err := domain.RehydrateLedgerEntry(p)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})

	t.Run("rejects a persisted line with no direction", func(t *testing.T) {
		p := ledgerParams(t)
		p.Direction = ""
		_, err := domain.RehydrateLedgerEntry(p)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrLedgerArithmetic)
	})

	t.Run("does not re-derive the balance — a committed line is immutable", func(t *testing.T) {
		// Rehydration deliberately preserves whatever was committed, even if
		// the arithmetic would look odd in isolation. Re-deriving here would
		// silently rewrite history instead of surfacing corruption.
		p := ledgerParams(t)
		p.BalanceAfter = mustParse(t, "999.00", "BRL")
		e, err := domain.RehydrateLedgerEntry(p)
		require.NoError(t, err)
		assert.Equal(t, "999.00", e.BalanceAfter().String())
	})
}

// ── NewWagerTransactionPendingReferenceEvent ───────────────────────────────

func TestNewWagerTransactionPendingReferenceEvent(t *testing.T) {
	tx := pendingReferenceTx(t)

	env, err := domain.NewWagerTransactionPendingReferenceEvent(tx)
	require.NoError(t, err)

	assert.Equal(t, domain.EventWagerTransactionPendingReference, env.EventType)
	assert.Equal(t, domain.AggregateWagerTransaction, env.AggregateType)
	assert.Equal(t, tx.ID().String(), env.AggregateID)
	assert.Equal(t, tx.CorrelationID(), env.CorrelationID)
	assert.Equal(t, domain.EventSchemaVersion, env.Version)
	assert.NotEmpty(t, env.EventID)
	assert.False(t, env.OccurredAt.IsZero())

	data, ok := env.Data.(domain.WagerTransactionPendingReferenceData)
	require.True(t, ok, "envelope must carry the typed snapshot")
	assert.Equal(t, tx.ID().String(), data.TransactionID)
	assert.Equal(t, string(domain.KindRefund), data.Kind)
	assert.Equal(t, "provider-a", data.ProviderID)
	assert.Equal(t, "ext-1", data.ExternalTransactionID)
	assert.Equal(t, "ext-ref", data.ReferenceExternalTransactionID)
	assert.Equal(t, tx.PlayerID().String(), data.PlayerID)
	assert.Equal(t, tx.WalletID().String(), data.WalletID)
	assert.Equal(t, "10.00", data.Money.Amount)
	assert.Equal(t, "BRL", data.Money.Currency)
	assert.Equal(t, tx.PendingAttempts(), data.Attempts)

	t.Run("serializes money as a decimal string, never a float", func(t *testing.T) {
		raw, err := domain.MarshalEnvelope(env)
		require.NoError(t, err)

		assert.Contains(t, string(raw), `"amount":"10.00"`)
		assert.NotContains(t, string(raw), "10.0,")
		// A float carrying 10.00 would serialize as 10 or 10.0, never as a
		// quoted decimal string. Assert the quoted form explicitly.
		assert.NotContains(t, string(raw), `"amount":10`)
		assert.NotContains(t, string(raw), `"amount":1e`)

		var back map[string]any
		require.NoError(t, json.Unmarshal(raw, &back))
		assert.Equal(t, env.EventID, back["eventId"])
		assert.Equal(t, domain.EventWagerTransactionPendingReference, back["eventType"])
		assert.Equal(t, float64(domain.EventSchemaVersion), back["version"])
	})

	t.Run("carries the attempt count so redelivery is observable", func(t *testing.T) {
		require.NoError(t, tx.RetryPendingReference(baseTime.Add(2*time.Minute)))
		second, err := domain.NewWagerTransactionPendingReferenceEvent(tx)
		require.NoError(t, err)
		body := second.Data.(domain.WagerTransactionPendingReferenceData)
		assert.Equal(t, 2, body.Attempts)
	})
}

// ── Rehydration preserves ordering and identity ────────────────────────────

func TestRehydratePreservesMonotonicTimestamps(t *testing.T) {
	p := rehydrateParams(t)
	p.CreatedAt = baseTime
	p.UpdatedAt = baseTime.Add(37 * time.Minute)

	tx, err := domain.RehydrateWagerTransaction(p)
	require.NoError(t, err)
	assert.False(t, tx.UpdatedAt().Before(tx.CreatedAt()),
		"a row read back from the database must not run backwards in time")
	assert.Equal(t, 37*time.Minute, tx.UpdatedAt().Sub(tx.CreatedAt()))
}

func TestRehydratedTransactionKeepsItsIdentity(t *testing.T) {
	p := rehydrateParams(t)
	tx, err := domain.RehydrateWagerTransaction(p)
	require.NoError(t, err)
	assert.Equal(t, p.ID, tx.ID(),
		"rehydration must not mint a new identity — replays address the same row")
	assert.NotEqual(t, domain.MustNewID(), tx.ID())
}
