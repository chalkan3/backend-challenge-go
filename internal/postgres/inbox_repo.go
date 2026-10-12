package postgres

import (
	"context"
	"time"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

type inboxRepo struct{ ex querier }

// Begin records the durable receipt of a message. The insert and the domain
// effects share the caller's transaction, so a committed inbox row means the
// message was fully handled; a redelivery finds it and skips.
func (r *inboxRepo) Begin(ctx context.Context, consumerName, messageID, hash string) (service.InboxOutcome, error) {
	tag, err := r.ex.Exec(ctx, `
		INSERT INTO consumer_inbox (consumer_name, message_id, message_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		consumerName, messageID, hash)
	if err != nil {
		return service.InboxOutcome{}, err
	}
	if tag.RowsAffected() == 1 {
		return service.InboxOutcome{Inserted: true}, nil
	}
	var existingHash string
	var completed *time.Time
	err = r.ex.QueryRow(ctx, `
		SELECT message_hash, completed_at FROM consumer_inbox
		WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID).Scan(&existingHash, &completed)
	if err != nil {
		return service.InboxOutcome{}, err
	}
	return service.InboxOutcome{
		Exists:    true,
		HashMatch: existingHash == hash,
		Completed: completed != nil,
	}, nil
}

func (r *inboxRepo) Complete(ctx context.Context, consumerName, messageID string, at time.Time) error {
	_, err := r.ex.Exec(ctx, `
		UPDATE consumer_inbox SET completed_at = $3
		WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID, at)
	return err
}
