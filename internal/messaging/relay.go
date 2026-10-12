package messaging

import (
	"context"
	"log/slog"
	"time"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// OutboxWorker publishes committed integration events. Multiple instances
// dispute rows via FOR UPDATE SKIP LOCKED; abandoned rows (crash between
// claim and publish) are recovered when the lease expires.
type OutboxWorker struct {
	txm       service.TxManager
	publisher *sqs.Publisher
	cfg       *config.Config
	log       *slog.Logger
	hook      Instrumentation
}

// NewOutboxWorker wires the relay.
func NewOutboxWorker(txm service.TxManager, publisher *sqs.Publisher, cfg *config.Config, log *slog.Logger, hook Instrumentation) *OutboxWorker {
	return &OutboxWorker{txm: txm, publisher: publisher, cfg: cfg, log: log.With(slog.String("worker", "outbox")), hook: hook}
}

// Run blocks until ctx is cancelled.
func (w *OutboxWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.OutboxInterval)
	defer ticker.Stop()
	w.log.Info("outbox publisher started",
		slog.Duration("interval", w.cfg.OutboxInterval), slog.Int("batch", w.cfg.OutboxBatch))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox publisher stopped")
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *OutboxWorker) tick(ctx context.Context) {
	r := w.txm.Repos()
	now := time.Now().UTC()
	items, err := r.Outbox().ClaimBatch(ctx, now, w.cfg.OutboxLease, w.cfg.OutboxBatch)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("outbox claim failed", slog.String("error", err.Error()))
		}
		return
	}
	for _, item := range items {
		if err := w.publisher.Publish(ctx, item); err != nil {
			// Retry with exponential backoff capped at 1h; the lease also
			// expires and another instance can pick the row up.
			next := now.Add(ExpBackoff(item.Attempts, time.Hour))
			if mErr := r.Outbox().MarkFailed(ctx, item.EventID, next, err.Error()); mErr != nil {
				w.log.Error("outbox mark failed", slog.String("error", mErr.Error()))
			}
			w.hook.OutboxFailed()
			w.log.Warn("outbox publish failed, scheduled for retry",
				slog.String("eventId", item.EventID.String()),
				slog.Int("attempts", item.Attempts),
				slog.String("error", err.Error()))
			continue
		}
		if err := r.Outbox().MarkPublished(ctx, item.EventID, time.Now().UTC()); err != nil {
			// Already published: republication preserves eventId, SQS
			// deduplication absorbs it.
			w.log.Warn("outbox mark published failed", slog.String("error", err.Error()))
		}
		w.hook.OutboxPublished()
	}
	if pending, lag, err := r.Outbox().Stats(ctx); err == nil {
		w.hook.OutboxStats(pending, lag)
	}
}

// expBackoff computes the exponential retry delay 1s, 2s, 4s, ... capped at
// limit. It is a pure function so unit tests can pin the progression.
func ExpBackoff(attempts int, limit time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts
	if shift > 20 {
		shift = 20
	}
	d := (1 << uint(shift)) * time.Second
	if d > limit || d <= 0 {
		return limit
	}
	return d
}
