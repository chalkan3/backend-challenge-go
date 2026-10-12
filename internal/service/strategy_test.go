package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// These tests exercise the StrategyRegistry extension seam independently of
// the shared pipeline: registration, lookup, override, and the Open/Closed
// guarantee that an unregistered kind fails closed.

func TestStrategyRegistry_RegisterAndLookup(t *testing.T) {
	reg := NewStrategyRegistry(2)
	called := ""
	reg.Register(domain.KindBet, func(context.Context, Repos, *domain.WagerTransaction, *domain.Wallet) (WagerResult, error) {
		called = "bet"
		return WagerResult{}, nil
	})

	s, ok := reg.Lookup(domain.KindBet)
	require.True(t, ok)
	_, err := s(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "bet", called)
	assert.ElementsMatch(t, []domain.TxKind{domain.KindBet}, reg.Kinds())
}

func TestStrategyRegistry_UnregisteredFailsClosed(t *testing.T) {
	reg := NewStrategyRegistry(0)
	_, ok := reg.Lookup(domain.KindWin)
	assert.False(t, ok, "an unregistered kind must not resolve")
}

func TestStrategyRegistry_RegisterOverrides(t *testing.T) {
	reg := NewStrategyRegistry(1)
	mk := func(string) wagerStrategy {
		return func(context.Context, Repos, *domain.WagerTransaction, *domain.Wallet) (WagerResult, error) {
			return WagerResult{Transaction: nil, WalletBalance: domain.ZeroMoneyMust("BRL")}, nil
		}
	}
	reg.Register(domain.KindBet, mk("first"))
	reg.Register(domain.KindBet, mk("second"))
	assert.Len(t, reg.Kinds(), 1, "overriding keeps a single entry per kind")
}

// TestDefaultRegistry_CoversAllExternalKinds pins that every externally
// processable kind has a strategy and that OPENING (internal-only) does not.
func TestDefaultRegistry_CoversAllExternalKinds(t *testing.T) {
	svc := &WagerService{}
	reg := svc.defaultRegistry()

	for _, k := range []domain.TxKind{
		domain.KindBet, domain.KindWin, domain.KindLoss,
		domain.KindRefund, domain.KindRollback,
	} {
		_, ok := reg.Lookup(k)
		assert.True(t, ok, "kind %s must have a strategy", k)
	}
	_, ok := reg.Lookup(domain.KindOpening)
	assert.False(t, ok, "OPENING is internal-only and must have no external strategy")
	assert.Len(t, reg.Kinds(), 5)
}

// TestWithStrategyRegistry_OverridesBuiltins proves the functional option is
// a real extension seam: a custom registry replaces the built-in dispatch.
func TestWithStrategyRegistry_OverridesBuiltins(t *testing.T) {
	custom := NewStrategyRegistry(0)
	svc := NewWagerService(nil, nil, nil, WithStrategyRegistry(custom))
	assert.Empty(t, svc.strategies.Kinds())

	// a nil registry must not clobber the built-ins
	svc2 := NewWagerService(nil, nil, nil, WithStrategyRegistry(nil))
	assert.Len(t, svc2.strategies.Kinds(), 5)
}
