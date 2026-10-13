// Package domain_test — accessor and validation-branch coverage for
// transaction.go.
//
// The getters look like boilerplate, but they are the domain's read API: the
// postgres adapter scans a row, rehydrates the aggregate and then reads it
// back through exactly these methods. A wrong dereference (returning the
// pointer instead of the value, or panicking on nil) would corrupt every read
// path while the write tests stayed green. So they get pinned, cheaply.
//
// The error branches of NewOpeningTransaction and the transition methods are
// here for the same reason: they are the guards that keep an invalid state
// from ever reaching the database.
package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// ── ParseTxKind ────────────────────────────────────────────────────────────

// TestParseTxKind walks the closed set and the rejection. It matters because
// ParseTxKind is what turns a wire string (JSON, SQS body, DB column) into a
// domain kind: an unknown string must be an error, never a silently-accepted
// zero value.
func TestParseTxKind(t *testing.T) {
	for _, want := range []domain.TxKind{
		domain.KindOpening, domain.KindBet, domain.KindWin,
		domain.KindLoss, domain.KindRefund, domain.KindRollback,
	} {
		got, err := domain.ParseTxKind(string(want))
		require.NoErrorf(t, err, "parsing %q", want)
		assert.Equalf(t, want, got, "round-trip of %q", want)
	}

	for _, bad := range []string{"", "bet", "UNKNOWN", "BET "} {
		_, err := domain.ParseTxKind(bad)
		assert.Errorf(t, err, "parsing %q must fail", bad)
	}
}

// ── Accessors on a fully-populated transaction ─────────────────────────────

// fullyPopulated builds a REFUND with every nullable column set, so each
// *Ptr accessor has something to return. The pointer accessors exist for the
// adapter's INSERT (pgx needs a *string to encode NULL); a nil-returning bug
// here would drop the column silently.
func fullyPopulated(t *testing.T) *domain.WagerTransaction {
	t.Helper()

	refTx := domain.MustNewID()
	p := domain.RehydrateParams{
		ID:                    domain.MustNewID(),
		Kind:                  domain.KindRefund,
		Origin:                domain.OriginExternal,
		ProviderID:            strPtr("provider-a"),
		ExternalTxID:          strPtr("ext-1"),
		IdempotencyKey:        strPtr("idem-1"),
		PayloadHash:           strPtr("hash-1"),
		PlayerID:              domain.MustNewID(),
		WalletID:              domain.MustNewID(),
		RoundID:               strPtr("round-1"),
		GameID:                strPtr("game-1"),
		Currency:              "BRL",
		AmountMinor:           10_00,
		ReferenceExternalTxID: strPtr("ext-ref"),
		ReferenceTxID:         &refTx,
		Status:                domain.StatusRejected,
		FailureCode:           strPtr("INSUFFICIENT_FUNDS"),
		FailureMessage:        strPtr("balance too low"),
		ResultDirection:       domain.DirectionDebit,
		ResultAmountMinor:     10_00,
		BalanceBeforeMinor:    i64ptr(100_00),
		BalanceAfterMinor:     i64ptr(90_00),
		WalletVersionAfter:    i64ptr(7),
		CorrelationID:         "corr-1",
		ReceivedVia:           domain.ViaSQS,
		PendingAttempts:       3,
		CreatedAt:             baseTime,
		UpdatedAt:             baseTime.Add(time.Second),
	}
	tx, err := domain.RehydrateWagerTransaction(p)
	require.NoError(t, err)
	return tx
}

func i64ptr(v int64) *int64 { return &v }

// TestWagerTransactionAccessors pins every exported reader against the values
// that went in, including the nil-sensitive ones.
func TestWagerTransactionAccessors(t *testing.T) {
	tx := fullyPopulated(t)

	assert.Equal(t, "provider-a", tx.ProviderID())
	assert.Equal(t, "ext-1", tx.ExternalTxID())
	assert.Equal(t, "idem-1", tx.IdempotencyKey())
	assert.Equal(t, "hash-1", tx.PayloadHash())
	assert.Equal(t, "round-1", tx.RoundID())
	assert.Equal(t, "game-1", tx.GameID())
	assert.Equal(t, "BRL", tx.Currency())
	assert.Equal(t, "ext-ref", tx.ReferenceExternalTxID())
	require.NotNil(t, tx.ReferenceTxID())
	assert.Equal(t, domain.StatusRejected, tx.Status())
	assert.Equal(t, "INSUFFICIENT_FUNDS", tx.FailureCode())
	assert.Equal(t, "balance too low", tx.FailureMessage())
	assert.Equal(t, domain.DirectionDebit, tx.ResultDirection())
	assert.Equal(t, domain.ViaSQS, tx.ReceivedVia())
	assert.Equal(t, 3, tx.PendingAttempts())

	// The pointer accessors must return the same address-space view: non-nil
	// and equal to the values above. This is the contract the adapter relies
	// on to encode NULLs correctly.
	assert.NotNil(t, tx.ProviderIDPtr())
	assert.Equal(t, "provider-a", *tx.ProviderIDPtr())
	assert.NotNil(t, tx.ExternalTxIDPtr())
	assert.NotNil(t, tx.IdempotencyKeyPtr())
	assert.NotNil(t, tx.PayloadHashPtr())
	assert.NotNil(t, tx.RoundIDPtr())
	assert.NotNil(t, tx.GameIDPtr())
	assert.NotNil(t, tx.ReferenceExternalTxIDPtr())
	assert.NotNil(t, tx.FailureCodePtr())
	assert.NotNil(t, tx.FailureMessagePtr())

	// Money and version accessors.
	require.NotNil(t, tx.BalanceBefore())
	require.NotNil(t, tx.BalanceAfter())
	require.NotNil(t, tx.WalletVersionAfter())
	assert.Equal(t, int64(100_00), tx.BalanceBefore().AmountMinor())
	assert.Equal(t, int64(90_00), tx.BalanceAfter().AmountMinor())
	assert.Equal(t, int64(7), *tx.WalletVersionAfter())
}

// TestAccessorsAreNilSafeOnAFreshTransaction is the other half: a transaction
// that never went through the optional columns must return zero values, not
// panic. Dereferencing a nil here would take down the whole read path.
func TestAccessorsAreNilSafeOnAFreshTransaction(t *testing.T) {
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "p",
		ExternalTransactionID: "e",
		PlayerID:              domain.MustNewID(),
		WalletID:              domain.MustNewID(),
		Kind:                  domain.KindBet,
		Money:                 mustParse(t, "10.00", "BRL"),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "c",
		Now:                   baseTime,
	})
	require.NoError(t, err)

	assert.Equal(t, "", tx.RoundID())
	assert.Equal(t, "", tx.GameID())
	assert.Equal(t, "", tx.ReferenceExternalTxID())
	assert.Equal(t, "", tx.FailureCode())
	assert.Equal(t, "", tx.FailureMessage())
	assert.Nil(t, tx.ReferenceTxID())
	assert.Nil(t, tx.BalanceBefore())
	assert.Nil(t, tx.BalanceAfter())
	assert.Nil(t, tx.WalletVersionAfter())
}

// ── SetReference ───────────────────────────────────────────────────────────

// TestSetReference pins both halves of the rule: REFUND/ROLLBACK accept an
// internal reference, and every other kind is refused. Letting a BET carry a
// reference would let a reversal be built from an operation that can never be
// reversed.
func TestSetReference(t *testing.T) {
	refID := domain.MustNewID()

	t.Run("REFUND accepts a reference", func(t *testing.T) {
		tx := externalTx(t, func(p *domain.ExternalTxParams) {
			p.Kind = domain.KindRefund
			p.ReferenceExternalTxID = "ext-ref"
		})
		require.NoError(t, tx.SetReference(refID))
		require.NotNil(t, tx.ReferenceTxID())
		assert.Equal(t, refID, *tx.ReferenceTxID())
	})

	t.Run("ROLLBACK accepts a reference", func(t *testing.T) {
		tx := externalTx(t, func(p *domain.ExternalTxParams) {
			p.Kind = domain.KindRollback
			p.ReferenceExternalTxID = "ext-ref"
		})
		require.NoError(t, tx.SetReference(refID))
		assert.Equal(t, refID, *tx.ReferenceTxID())
	})

	t.Run("BET refuses a reference", func(t *testing.T) {
		tx := externalTx(t, nil) // BET
		err := tx.SetReference(refID)
		require.Error(t, err)
		assert.Nil(t, tx.ReferenceTxID(), "a rejected SetReference must not mutate the aggregate")
	})
}

// ── Constructor guards ─────────────────────────────────────────────────────

// TestNewOpeningTransactionGuards exercises the three rejections. An opening
// movement is the seed of every balance, so a negative or mismatched-currency
// seed must never be constructible.
func TestNewOpeningTransactionGuards(t *testing.T) {
	player, wallet := domain.MustNewID(), domain.MustNewID()

	t.Run("accepts a valid opening", func(t *testing.T) {
		tx, err := domain.NewOpeningTransaction(player, wallet, "BRL", mustParse(t, "100.00", "BRL"), baseTime)
		require.NoError(t, err)
		assert.Equal(t, domain.KindOpening, tx.Kind())
		assert.Equal(t, domain.OriginInternal, tx.Origin())
	})

	t.Run("a malformed currency cannot even be represented", func(t *testing.T) {
		// The guard is unreachable through this constructor on purpose: a
		// Money in a malformed currency cannot be built in the first place,
		// so NewOpeningTransaction's own ValidateCurrency is defence in
		// depth. We assert the real barrier (NewMoney) instead of pretending
		// the inner branch is reachable.
		_, err := domain.NewMoney(100_00, "BR")
		assert.Error(t, err)
	})

	t.Run("rejects a currency mismatch between wallet and seed", func(t *testing.T) {
		// Well-formed on both sides, but they disagree.
		_, err := domain.NewOpeningTransaction(player, wallet, "BRL", mustParse(t, "100.00", "USD"), baseTime)
		assert.ErrorIs(t, err, domain.ErrCurrencyMismatch)
	})

	t.Run("rejects a negative opening balance", func(t *testing.T) {
		_, err := domain.NewOpeningTransaction(player, wallet, "BRL", mustParse(t, "-1.00", "BRL"), baseTime)
		assert.ErrorIs(t, err, domain.ErrInvalidAmount)
	})
}

// TestRehydrateRejectsAMalformedCurrency documents where the currency barrier
// actually is. Every Money in a rehydrated aggregate is built with p.Currency,
// so a currency mismatch is not representable — the row would have to hold a
// non-3-letter code, and NewMoney refuses it first. The test pins the
// reachable rejection rather than a branch that cannot be reached.
func TestRehydrateRejectsAMalformedCurrency(t *testing.T) {
	p := domain.RehydrateParams{
		ID:                domain.MustNewID(),
		Kind:              domain.KindBet,
		Origin:            domain.OriginExternal,
		PlayerID:          domain.MustNewID(),
		WalletID:          domain.MustNewID(),
		Currency:          "BR", // not a 3-letter code
		AmountMinor:       10_00,
		Status:            domain.StatusProcessed,
		ResultDirection:   domain.DirectionDebit,
		ResultAmountMinor: 10_00,
		CorrelationID:     "c",
		ReceivedVia:       domain.ViaHTTP,
		CreatedAt:         baseTime,
		UpdatedAt:         baseTime,
	}
	_, err := domain.RehydrateWagerTransaction(p)
	assert.Error(t, err, "a non-ISO currency must be refused at rehydration")
}

// TestTerminalTransitionRequiresFailureCode pins the invariant that a REJECTED
// or FAILED transaction must name why. A terminal state with no reason is
// unoperable: nobody can triage it.
func TestTerminalTransitionRequiresFailureCode(t *testing.T) {
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "p",
		ExternalTransactionID: "e",
		PlayerID:              domain.MustNewID(),
		WalletID:              domain.MustNewID(),
		Kind:                  domain.KindBet,
		Money:                 mustParse(t, "10.00", "BRL"),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "c",
		Now:                   baseTime,
	})
	require.NoError(t, err)

	// Empty failure code is refused...
	err = tx.MarkRejected("", "no reason", baseTime)
	require.Error(t, err, "a terminal state without a code must be refused")
	// ...and a well-formed one is accepted.
	require.NoError(t, tx.MarkRejected("INSUFFICIENT_FUNDS", "balance too low", baseTime))
	assert.Equal(t, domain.StatusRejected, tx.Status())
	assert.Equal(t, "INSUFFICIENT_FUNDS", tx.FailureCode())
}
