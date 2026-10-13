package messaging

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// ReconcileWorker continuously verifies that every wallet's stored balance
// still equals the sum of its ledger. Reconcile is a pure read, so a pass
// never blocks a wager: it walks wallets in keyset batches and, for each one,
// delegates to WalletService.Reconcile — the same use case the on-demand
// HTTP route calls. There is no second implementation of the arithmetic to
// drift from the first.
//
// The worker exists because on-demand reconciliation only finds a divergence
// if someone asks about that wallet. Damage done while nobody was looking
// (a bad deploy, a manual UPDATE) would sit undetected until a client
// happened to reconcile. A periodic sweep turns detection from opt-in into
// a property of the running system, and reconciliation_divergences_total
// turns it into an alertable signal.
//
// It is read-only on purpose: it reports and instruments, it never repairs.
// Silent auto-correction would be a second way to write balances, and the
// ledger is the only source of truth — a divergence is a fact to surface,
// not a number to paper over.
type ReconcileWorker struct {
	txm  service.TxManager
	svc  *service.WalletService
	cfg  *config.Config
	log  *slog.Logger
	hook Instrumentation
}

// NewReconcileWorker wires the sweeper.
func NewReconcileWorker(txm service.TxManager, svc *service.WalletService, cfg *config.Config, log *slog.Logger, hook Instrumentation) *ReconcileWorker {
	return &ReconcileWorker{
		txm:  txm,
		svc:  svc,
		cfg:  cfg,
		log:  log.With(slog.String("worker", "reconcile")),
		hook: hook,
	}
}

// Run blocks until ctx is cancelled.
func (w *ReconcileWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.ReconcileInterval)
	defer ticker.Stop()
	w.log.Info("reconciliation worker started",
		slog.Duration("interval", w.cfg.ReconcileInterval), slog.Int("batch", w.cfg.ReconcileBatch))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("reconciliation worker stopped")
			return nil
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep walks every wallet once, in keyset batches. A single wallet that
// fails to reconcile (deleted between pages, transient read error) is logged
// and skipped: it must not abort the pass and leave the rest of the table
// unverified.
func (w *ReconcileWorker) sweep(ctx context.Context) {
	ctx, span := w.hook.StartSpan(ctx, SpanReconcileSweep)
	defer span.End()

	var checked, divergences int
	var after *uuid.UUID

	for {
		if ctx.Err() != nil {
			return // shutdown mid-pass: stop cleanly, the next tick starts over
		}
		ids, err := w.page(ctx, after)
		if err != nil {
			span.RecordError(err)
			w.log.Warn("reconciliation page failed", slog.String("error", err.Error()))
			break
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			report, err := w.svc.Reconcile(ctx, id.String())
			if err != nil {
				// A wallet deleted between the page listing and its check is a
				// lifecycle race, not a divergence: Reconcile maps the
				// repository's ErrRepoNotFound into a WALLET_NOT_FOUND
				// NotFoundError, so that code (not the repo sentinel) is what
				// identifies the benign case.
				if service.NotFoundCode(err) == service.CodeWalletNotFound {
					continue
				}
				w.log.Warn("reconcile wallet failed",
					slog.String("walletId", id.String()), slog.String("error", err.Error()))
				continue
			}
			checked++
			if !report.Consistent {
				// WalletService.Reconcile already emitted the divergence
				// counter and the error log for this wallet; here we only
				// tally it for the sweep summary.
				divergences++
			}
		}
		last := ids[len(ids)-1]
		after = &last
		if len(ids) < w.cfg.ReconcileBatch {
			break
		}
	}

	span.SetAttributes(Attr(AttrReconcileSize, int64(checked)), Attr(AttrDivergences, int64(divergences)))
	w.hook.ReconciliationSweep(checked)
	if divergences > 0 {
		w.log.Error("reconciliation sweep found divergences",
			slog.Int("checked", checked), slog.Int("divergences", divergences))
		return
	}
	w.log.Info("reconciliation sweep complete", slog.Int("checked", checked))
}

// page reads one keyset batch outside any transaction: reconciliation is a
// read, so it does not need the write transaction the use case opens per
// wallet, and holding one open across a whole sweep would only widen lock
// scope for no benefit.
func (w *ReconcileWorker) page(ctx context.Context, after *uuid.UUID) ([]uuid.UUID, error) {
	return w.txm.Repos().Wallets().ListIDs(ctx, after, w.cfg.ReconcileBatch)
}
