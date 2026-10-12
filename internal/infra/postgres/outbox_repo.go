package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chalkan33/wallet/internal/app"
	"github.com/chalkan33/wallet/internal/domain"
)

type outboxRepo struct{ ex querier }

func (r *outboxRepo) Enqueue(ctx context.Context, envelopes []domain.Envelope) error {
	if len(envelopes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range envelopes {
		payload, err := domain.MarshalEnvelope(e)
		if err != nil {
			return err
		}
		aggID, err := uuid.Parse(e.AggregateID)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO integration_outbox
				(event_id, aggregate_type, aggregate_id, event_type, event_version,
				 correlation_id, payload, available_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb, now())`,
			e.EventID, e.AggregateType, aggID, e.EventType, e.Version,
			e.CorrelationID, string(payload))
	}
	return r.ex.SendBatch(ctx, batch).Close()
}

// ClaimBatch leases unpublished, due events with FOR UPDATE SKIP LOCKED and
// bumps available_at by the lease: if this publisher dies before marking the
// event published, another instance recovers the row when the lease expires.
func (r *outboxRepo) ClaimBatch(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]app.OutboxItem, error) {
	rows, err := r.ex.Query(ctx, `
		UPDATE integration_outbox SET attempts = attempts + 1, available_at = $1 + $2
		WHERE event_id IN (
			SELECT event_id FROM integration_outbox
			WHERE published_at IS NULL AND available_at <= $1
			ORDER BY available_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING event_id, aggregate_id, event_type, attempts, payload::text`,
		now, lease, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.OutboxItem
	for rows.Next() {
		var it app.OutboxItem
		var aggID uuid.UUID
		if err := rows.Scan(&it.EventID, &aggID, &it.EventType, &it.Attempts, &it.Payload); err != nil {
			return nil, err
		}
		it.AggregateID = aggID.String()
		out = append(out, it)
	}
	return out, rows.Err()
}

func (r *outboxRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, at time.Time) error {
	_, err := r.ex.Exec(ctx,
		`UPDATE integration_outbox SET published_at = $2 WHERE event_id = $1 AND published_at IS NULL`,
		eventID, at)
	return err
}

// MarkFailed schedules the retry with exponential backoff (computed by the
// worker) and stores the last error for diagnosis.
func (r *outboxRepo) MarkFailed(ctx context.Context, eventID uuid.UUID, nextAvailable time.Time, lastErr string) error {
	_, err := r.ex.Exec(ctx, `
		UPDATE integration_outbox SET available_at = $2, last_error = $3
		WHERE event_id = $1 AND published_at IS NULL`,
		eventID, nextAvailable, lastErr)
	return err
}

func (r *outboxRepo) Stats(ctx context.Context) (pending int64, oldestAgeSeconds float64, err error) {
	err = r.ex.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at)), 0)
		FROM integration_outbox WHERE published_at IS NULL`).Scan(&pending, &oldestAgeSeconds)
	return pending, oldestAgeSeconds, err
}
