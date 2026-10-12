package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the Specification abstraction itself and the composed
// external-transaction rule set, proving that each named rule is individually
// testable and that composition preserves the published error order.

func TestSpecification_And_ShortCircuitsInOrder(t *testing.T) {
	var evaluated []string
	first := SpecFunc[int]{Desc: "first", Fn: func(int) error {
		evaluated = append(evaluated, "first")
		return ErrInvalidAmount.With("boom")
	}}
	second := SpecFunc[int]{Desc: "second", Fn: func(int) error {
		evaluated = append(evaluated, "second")
		return nil
	}}

	spec := And[int](first, second)
	err := spec.IsSatisfiedBy(1)

	require.Error(t, err)
	var de *Error
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, ErrInvalidAmount.Code, de.Code, "the returned error keeps its typed identity")
	assert.Equal(t, []string{"first"}, evaluated,
		"evaluation stops at the first violated rule")
}

func TestSpecification_And_EmptyIsAlwaysSatisfied(t *testing.T) {
	spec := And[int]()
	assert.NoError(t, spec.IsSatisfiedBy(42))
	assert.Equal(t, "always", spec.Describe())
}

func TestSpecification_And_DescribeComposesLabels(t *testing.T) {
	a := SpecFunc[int]{Desc: "a", Fn: func(int) error { return nil }}
	b := SpecFunc[int]{Desc: "b", Fn: func(int) error { return nil }}
	assert.Equal(t, "a AND b", And[int](a, b).Describe())
}

func TestSpecification_Not(t *testing.T) {
	always := SpecFunc[int]{Desc: "always", Fn: func(int) error { return nil }}
	spec := NotSpec[int]{Inner: always, Code: ErrInvalidAmount.Code, Msg: "must not hold"}

	err := spec.IsSatisfiedBy(0)
	require.Error(t, err)
	var de *Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, ErrInvalidAmount.Code, de.Code)
	assert.Equal(t, "NOT(always)", spec.Describe())
}

// TestExternalTransactionSpecification_RuleOrder pins the order contract:
// kind identity wins over missing metadata, which wins over the amount
// policy, which wins over the reference requirement.
func TestExternalTransactionSpecification_RuleOrder(t *testing.T) {
	brl := func(s string) Money { m, err := ParseMoney(s, "BRL"); require.NoError(t, err); return m }

	cases := []struct {
		name    string
		params  ExternalTxParams
		wantIs  func(error) bool
		wantMsg string
	}{
		{
			name:    "OPENING is internal-only and reported as invalid kind",
			params:  ExternalTxParams{Kind: KindOpening},
			wantIs:  func(e error) bool { return errors.Is(e, ErrInvalidKind) },
			wantMsg: "internal-only",
		},
		{
			name:    "unknown kind",
			params:  ExternalTxParams{Kind: TxKind("NOPE")},
			wantIs:  func(e error) bool { return errors.Is(e, ErrInvalidKind) },
			wantMsg: "unknown kind",
		},
		{
			name:    "missing provider wins over amount",
			params:  ExternalTxParams{Kind: KindBet, Money: brl("0.00")},
			wantIs:  func(e error) bool { return errors.Is(e, ErrInvalidTransaction) },
			wantMsg: "providerId and externalTransactionId are required",
		},
		{
			name: "LOSS must be zero",
			params: ExternalTxParams{Kind: KindLoss, ProviderID: "provider-a",
				ExternalTransactionID: "e1", Money: brl("1.00")},
			wantIs:  func(e error) bool { return errors.Is(e, ErrInvalidAmount) },
			wantMsg: "LOSS requires amount 0.00",
		},
		{
			name: "BET must be positive",
			params: ExternalTxParams{Kind: KindBet, ProviderID: "provider-a",
				ExternalTransactionID: "e1", Money: brl("0.00")},
			wantIs:  func(e error) bool { return errors.Is(e, ErrInvalidAmount) },
			wantMsg: "BET requires a positive amount",
		},
		{
			name: "REFUND requires a reference",
			params: ExternalTxParams{Kind: KindRefund, ProviderID: "provider-a",
				ExternalTransactionID: "e1", Money: brl("1.00")},
			wantIs:  func(e error) bool { return errors.Is(e, ErrMissingReference) },
			wantMsg: "referenceExternalTransactionId is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := externalTransactionSpecification.IsSatisfiedBy(tc.params)
			require.Error(t, err)
			assert.True(t, tc.wantIs(err), "unexpected error identity: %v", err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestExternalTransactionSpecification_AcceptsValidShapes(t *testing.T) {
	bet, err := ParseMoney("10.00", "BRL")
	require.NoError(t, err)
	zero, err := ParseMoney("0.00", "BRL")
	require.NoError(t, err)

	ok := []ExternalTxParams{
		{Kind: KindBet, ProviderID: "provider-a", ExternalTransactionID: "e1", Money: bet},
		{Kind: KindWin, ProviderID: "provider-a", ExternalTransactionID: "e2", Money: bet},
		{Kind: KindLoss, ProviderID: "provider-a", ExternalTransactionID: "e3", Money: zero},
		{Kind: KindRefund, ProviderID: "provider-a", ExternalTransactionID: "e4",
			ReferenceExternalTxID: "bet-1", Money: bet},
		{Kind: KindRollback, ProviderID: "provider-a", ExternalTransactionID: "e5",
			ReferenceExternalTxID: "bet-1", Money: bet},
	}
	for _, p := range ok {
		assert.NoError(t, externalTransactionSpecification.IsSatisfiedBy(p),
			"kind %s should be accepted", p.Kind)
	}
}

// TestNewExternalTransaction_SpecificationParity asserts that the refactor to
// a Specification chain did not change the observable behaviour or the error
// codes of the constructor.
func TestNewExternalTransaction_SpecificationParity(t *testing.T) {
	bet, err := ParseMoney("10.00", "BRL")
	require.NoError(t, err)

	_, err = NewExternalTransaction(ExternalTxParams{
		Kind: KindOpening, ProviderID: "p", ExternalTransactionID: "e", Money: bet,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidKind))

	tx, err := NewExternalTransaction(ExternalTxParams{
		Kind: KindBet, ProviderID: "provider-a", ExternalTransactionID: "e9",
		PlayerID: uuid.New(), WalletID: uuid.New(), RoundID: "r", GameID: "g",
		Money: bet, Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.Equal(t, StatusPending, tx.Status())
	assert.Equal(t, OriginExternal, tx.Origin())
}
