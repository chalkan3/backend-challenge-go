//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// sweepHook implements the worker's messaging.Instrumentation. It tallies what
// the worker reports (checked wallets and completed sweeps) so the test can
// assert on counts rather than on logs.
type sweepHook struct {
	mu         sync.Mutex
	checked    int
	divergent  int
	sweepCount int
}

func (h *sweepHook) ReconciliationSweep(checked int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepCount++
	h.checked += checked
}

// noteDivergence records one mismatch from the use case's port: the single
// counter the metric is built on.
func (h *sweepHook) noteDivergence() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.divergent++
}

// messaging.Instrumentation — inert here; the reconcile worker only calls
// ReconciliationSweep and StartSpan.
func (h *sweepHook) OutboxPublished()           {}
func (h *sweepHook) OutboxFailed()              {}
func (h *sweepHook) OutboxStats(int64, float64) {}
func (h *sweepHook) StartSpan(ctx context.Context, _ string, _ ...messaging.SpanAttr) (context.Context, messaging.Span) {
	return ctx, silentOutboxSpan{}
}

var _ messaging.Instrumentation = (*sweepHook)(nil)

// sweepServeHook satisfies the use case's service.Instrumentation. Go has no
// method overloading, and the two ports carry different attribute types, so the
// service and the worker cannot share one value's StartSpan. It forwards the
// divergence signal into the same hook the worker reports through, so the test
// observes every mismatch exactly once.
type sweepServeHook struct {
	silentInstrumentation
	hook *sweepHook
}

func (h sweepServeHook) ReconciliationDivergence() { h.hook.noteDivergence() }

var _ service.Instrumentation = (*sweepServeHook)(nil)

// TestContinuousReconciliationFindsPlantedDivergence proves the worker against
// the real database: a wallet whose stored balance was mutated behind the
// ledger's back is detected by an on-going sweep, while a consistent wallet is
// not flagged. The divergence is planted with a direct UPDATE — the exact
// class of damage (bad deploy, manual fix) that a periodic sweep exists to
// catch, and that on-demand reconciliation would only find if someone asked
// about that specific wallet.
func TestContinuousReconciliationFindsPlantedDivergence(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()

	// Two real wallets, with a real opening ledger row each.
	consistentID := openWalletDirect(t, newPlayerID(t), "100.00")
	divergentID := openWalletDirect(t, newPlayerID(t), "100.00")

	// Plant the divergence: the stored balance no longer matches the ledger.
	// The ledger stays untouched (the append-only trigger forbids editing it),
	// so it remains the source of truth.
	tag, err := pool.Exec(ctx,
		`UPDATE wallets SET balance_minor = balance_minor + 500 WHERE id = $1`,
		uuid.MustParse(divergentID))
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	// The worker runs in-process against the same database the app uses —
	// the same repositories, the same SQL, the same Reconcile use case.
	store := postgres.NewStore(pool, nil)
	hook := &sweepHook{}
	svc := service.NewWalletService(store, testLogger, &sweepServeHook{hook: hook})
	cfg := &config.Config{ReconcileInterval: 200 * time.Millisecond, ReconcileBatch: 500}
	worker := messaging.NewReconcileWorker(store, svc, cfg, testLogger, hook)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()

	// Wait until at least one full pass has run, then stop.
	pollUntil(t, 5*time.Second, func() bool {
		hook.mu.Lock()
		defer hook.mu.Unlock()
		return hook.sweepCount >= 1
	}, "worker never completed a sweep")
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}

	hook.mu.Lock()
	defer hook.mu.Unlock()
	// The sweep walks every wallet, so both our wallets were checked, and the
	// planted one — and only it — was counted divergent. (The whole table is
	// swept, so totals are >= ours; the divergence count is what we anchor on,
	// and a clean database keeps it at exactly one.)
	assert.GreaterOrEqual(t, hook.checked, 2, "both seeded wallets must be visited")
	assert.GreaterOrEqual(t, hook.divergent, 1, "the planted divergence must be found")

	// And the positive control: after repairing the balance, a fresh sweep
	// sees no divergence for that wallet.
	_, err = pool.Exec(ctx,
		`UPDATE wallets SET balance_minor = balance_minor - 500 WHERE id = $1`,
		uuid.MustParse(divergentID))
	require.NoError(t, err)

	report, err := svc.Reconcile(ctx, divergentID)
	require.NoError(t, err)
	assert.True(t, report.Consistent, "repaired wallet must reconcile clean")

	report, err = svc.Reconcile(ctx, consistentID)
	require.NoError(t, err)
	assert.True(t, report.Consistent, "untouched wallet must stay consistent")
}
