package messaging

import (
	"context"
	"log/slog"
	"time"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// ReferenceWorker resolves PENDING_REFERENCE transactions durably. Rows are
// claimed with SKIP LOCKED inside the same SQL transaction that persists the
// resolution outcome.
type ReferenceWorker struct {
	txm  service.TxManager
	svc  *service.WagerService
	cfg  *config.Config
	log  *slog.Logger
	hook Instrumentation
}

func NewReferenceWorker(txm service.TxManager, svc *service.WagerService, cfg *config.Config, log *slog.Logger, hook Instrumentation) *ReferenceWorker {
	return &ReferenceWorker{txm: txm, svc: svc, cfg: cfg, log: log.With(slog.String("worker", "references")), hook: hook}
}

func (w *ReferenceWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.ReferenceInterval)
	defer ticker.Stop()
	w.log.Info("reference worker started")
	for {
		select {
		case <-ctx.Done():
			w.log.Info("reference worker stopped")
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *ReferenceWorker) tick(ctx context.Context) {
	ctx, span := w.hook.StartSpan(ctx, SpanReferenceTick)
	defer span.End()

	err := w.txm.WithinTx(ctx, func(ctx context.Context, r service.Repos) error {
		claimed, err := r.Transactions().ClaimPendingReferences(ctx, time.Now().UTC(), w.cfg.ReferenceBatch)
		if err != nil {
			return err
		}
		span.SetAttributes(Attr(AttrReferenceSize, int64(len(claimed))))
		for _, t := range claimed {
			if _, err := w.svc.ResolvePending(ctx, r, t); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		span.RecordError(err)
		w.log.Warn("reference tick failed", slog.String("error", err.Error()))
	}
}
