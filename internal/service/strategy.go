package service

import (
	"context"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// wagerStrategy is the Strategy contract for one transaction kind. It runs
// after the shared pipeline has validated the input and locked the wallet.
type wagerStrategy func(
	ctx context.Context, r Repos,
	t *domain.WagerTransaction, w *domain.Wallet,
) (WagerResult, error)

// StrategyRegistry maps a domain kind to its Strategy. It is the extension
// point of the wager use case: adding a kind means registering a strategy,
// not editing the shared pipeline (Open/Closed). Lookup is explicit
// (comma-ok) so an unregistered kind fails closed with a validation error
// rather than panicking.
type StrategyRegistry struct {
	strategies map[domain.TxKind]wagerStrategy
}

// NewStrategyRegistry builds an empty registry with capacity for n kinds.
func NewStrategyRegistry(n int) *StrategyRegistry {
	return &StrategyRegistry{strategies: make(map[domain.TxKind]wagerStrategy, n)}
}

// Register binds a kind to its strategy. Registering the same kind twice
// overwrites it, which keeps composition idempotent and test-friendly.
func (r *StrategyRegistry) Register(kind domain.TxKind, s wagerStrategy) *StrategyRegistry {
	r.strategies[kind] = s
	return r
}

// Lookup returns the strategy for kind and whether it is registered.
func (r *StrategyRegistry) Lookup(kind domain.TxKind) (wagerStrategy, bool) {
	s, ok := r.strategies[kind]
	return s, ok
}

// Kinds returns the registered kinds (used by documentation/telemetry).
func (r *StrategyRegistry) Kinds() []domain.TxKind {
	out := make([]domain.TxKind, 0, len(r.strategies))
	for k := range r.strategies {
		out = append(out, k)
	}
	return out
}

// defaultRegistry builds the registry with the five externally processable
// kinds, each delegating to the shared private pipeline of the service.
// BET/WIN share the movement strategy; LOSS is the zero-movement strategy;
// REFUND/ROLLBACK share the reversal strategy.
func (s *WagerService) defaultRegistry() *StrategyRegistry {
	return NewStrategyRegistry(5).
		Register(domain.KindBet, func(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
			return s.processMovement(ctx, r, t, w, nil, false)
		}).
		Register(domain.KindWin, func(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
			return s.processMovement(ctx, r, t, w, nil, false)
		}).
		Register(domain.KindLoss, func(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
			return s.processLoss(ctx, r, t, w, false)
		}).
		Register(domain.KindRefund, func(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
			return s.processReversal(ctx, r, t, w, false)
		}).
		Register(domain.KindRollback, func(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
			return s.processReversal(ctx, r, t, w, false)
		})
}
