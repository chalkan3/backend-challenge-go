package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

type walletOpenRepo struct{ ex querier }

// Record binds an idempotency key to the wallet it created. The insert runs
// inside the caller's transaction, so the key and the wallet (plus its
// OPENING ledger entry) become durable atomically: a committed key always
// points at a wallet that really exists.
func (r *walletOpenRepo) Record(ctx context.Context, key, payloadHash string, walletID uuid.UUID) error {
	_, err := r.ex.Exec(ctx, `
		INSERT INTO wallet_open_requests (idempotency_key, payload_hash, wallet_id)
		VALUES ($1, $2, $3)`,
		key, payloadHash, walletID)
	return mapInsertError(err)
}

// Lookup returns the wallet recorded under key and the hash of the payload
// that produced it, letting the caller distinguish a replay of the same
// request from a reuse of the key with a different body.
func (r *walletOpenRepo) Lookup(ctx context.Context, key string) (uuid.UUID, string, error) {
	var walletID uuid.UUID
	var payloadHash string
	err := r.ex.QueryRow(ctx, `
		SELECT wallet_id, payload_hash FROM wallet_open_requests
		WHERE idempotency_key = $1`, key).Scan(&walletID, &payloadHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", service.ErrRepoNotFound
		}
		return uuid.Nil, "", err
	}
	return walletID, payloadHash, nil
}
