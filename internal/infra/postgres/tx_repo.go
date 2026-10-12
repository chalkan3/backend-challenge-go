package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chalkan3/backend-challenge-go/internal/app"
	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

const txCols = `id, kind, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
	player_id, wallet_id, round_id, game_id, currency, amount_minor,
	reference_external_transaction_id, reference_transaction_id,
	status, failure_code, failure_message,
	result_direction, result_amount_minor, balance_before_minor, balance_after_minor, wallet_version_after,
	channel, correlation_id, pending_attempts, next_retry_at, created_at, updated_at`

type txRepo struct{ ex querier }

func (r *txRepo) Insert(ctx context.Context, t *domain.WagerTransaction) error {
	_, err := r.ex.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, kind, origin, provider_id, external_transaction_id, idempotency_key, payload_hash,
			player_id, wallet_id, round_id, game_id, currency, amount_minor,
			reference_external_transaction_id, reference_transaction_id,
			status, failure_code, failure_message,
			result_direction, result_amount_minor, balance_before_minor, balance_after_minor, wallet_version_after,
			channel, correlation_id, pending_attempts, next_retry_at, created_at, updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29
		)`,
		t.ID(), t.Kind(), t.Origin(), t.ProviderIDPtr(), t.ExternalTxIDPtr(),
		t.IdempotencyKeyPtr(), t.PayloadHashPtr(),
		t.PlayerID(), t.WalletID(), t.RoundIDPtr(), t.GameIDPtr(), t.Currency(), t.Amount().AmountMinor(),
		t.ReferenceExternalTxIDPtr(), t.ReferenceTxID(),
		t.Status(), t.FailureCodePtr(), t.FailureMessagePtr(),
		directionOrNull(t.ResultDirection()), t.ResultAmount().AmountMinor(),
		minorOrNull(t.BalanceBefore()), minorOrNull(t.BalanceAfter()), t.WalletVersionAfter(),
		t.ReceivedVia(), t.CorrelationID(), t.PendingAttempts(), t.NextRetryAt(),
		t.CreatedAt(), t.UpdatedAt())
	return mapInsertError(err)
}

// Save updates the mutable projection of a PENDING_REFERENCE row. The WHERE
// guard makes terminal transitions insert-only: a committed PROCESSED row can
// never be modified through this path.
func (r *txRepo) Save(ctx context.Context, t *domain.WagerTransaction) error {
	tag, err := r.ex.Exec(ctx, `
		UPDATE wager_transactions SET
			reference_transaction_id = $2,
			status = $3,
			failure_code = $4,
			failure_message = $5,
			result_direction = $6,
			result_amount_minor = $7,
			balance_before_minor = $8,
			balance_after_minor = $9,
			wallet_version_after = $10,
			pending_attempts = $11,
			next_retry_at = $12,
			updated_at = $13
		WHERE id = $1 AND status IN ('PENDING','PENDING_REFERENCE')`,
		t.ID(), t.ReferenceTxID(), t.Status(), t.FailureCodePtr(), t.FailureMessagePtr(),
		directionOrNull(t.ResultDirection()), t.ResultAmount().AmountMinor(),
		minorOrNull(t.BalanceBefore()), minorOrNull(t.BalanceAfter()), t.WalletVersionAfter(),
		t.PendingAttempts(), t.NextRetryAt(), t.UpdatedAt())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("transaction already terminal or missing")
	}
	return nil
}

func (r *txRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	return scanTx(r.ex.QueryRow(ctx, `SELECT `+txCols+` FROM wager_transactions WHERE id = $1`, id))
}

func (r *txRepo) GetByProviderAndKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	return scanTx(r.ex.QueryRow(ctx,
		`SELECT `+txCols+` FROM wager_transactions WHERE provider_id = $1 AND idempotency_key = $2`,
		providerID, key))
}

func (r *txRepo) GetByProviderAndExternal(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	return scanTx(r.ex.QueryRow(ctx,
		`SELECT `+txCols+` FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`,
		providerID, externalID))
}

func (r *txRepo) ExistsProcessedReversal(ctx context.Context, refTxID uuid.UUID, kind domain.TxKind) (bool, error) {
	var exists bool
	err := r.ex.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM wager_transactions
			WHERE reference_transaction_id = $1 AND kind = $2 AND status = 'PROCESSED')`,
		refTxID, kind).Scan(&exists)
	return exists, err
}

// ClaimPendingReferences locks due PENDING_REFERENCE rows with
// FOR UPDATE SKIP LOCKED: concurrent instances dispute without blocking or
// duplicating work.
func (r *txRepo) ClaimPendingReferences(ctx context.Context, now time.Time, limit int) ([]*domain.WagerTransaction, error) {
	rows, err := r.ex.Query(ctx, `
		SELECT `+txCols+` FROM wager_transactions
		WHERE status = 'PENDING_REFERENCE' AND next_retry_at <= $1
		ORDER BY next_retry_at
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.WagerTransaction
	for rows.Next() {
		t, err := scanTxRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTx(row pgx.Row) (*domain.WagerTransaction, error) {
	t, err := scanTxRows(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrRepoNotFound
		}
		return nil, err
	}
	return t, nil
}

func scanTxRows(row pgx.Row) (*domain.WagerTransaction, error) {
	var (
		id, playerID, walletID                                 uuid.UUID
		kind, origin, currency, status, channel, correlationID string
		providerID, externalTxID, idemKey, payloadHash         *string
		roundID, gameID                                        *string
		refExternalID                                          *string
		refTxID                                                *uuid.UUID
		failureCode, failureMessage                            *string
		resultDirection                                        *domain.Direction
		amountMinor, resultAmountMinor                         int64
		balanceBeforeMinor, balanceAfterMinor                  *int64
		walletVersionAfter                                     *int64
		pendingAttempts                                        int
		nextRetryAt                                            *time.Time
		createdAt, updatedAt                                   time.Time
	)
	if err := row.Scan(&id, &kind, &origin, &providerID, &externalTxID, &idemKey, &payloadHash,
		&playerID, &walletID, &roundID, &gameID, &currency, &amountMinor,
		&refExternalID, &refTxID,
		&status, &failureCode, &failureMessage,
		&resultDirection, &resultAmountMinor, &balanceBeforeMinor, &balanceAfterMinor, &walletVersionAfter,
		&channel, &correlationID, &pendingAttempts, &nextRetryAt, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	resultDir := domain.DirectionNone
	if resultDirection != nil {
		resultDir = *resultDirection
	}
	return domain.RehydrateWagerTransaction(domain.RehydrateParams{
		ID: id, Kind: domain.TxKind(kind), Origin: domain.TxOrigin(origin),
		ProviderID: providerID, ExternalTxID: externalTxID,
		IdempotencyKey: idemKey, PayloadHash: payloadHash,
		PlayerID: playerID, WalletID: walletID,
		RoundID: roundID, GameID: gameID,
		Currency: currency, AmountMinor: amountMinor,
		ReferenceExternalTxID: refExternalID,
		ReferenceTxID:         refTxID,
		Status:                domain.TxStatus(status),
		FailureCode:           failureCode, FailureMessage: failureMessage,
		ResultDirection: resultDir, ResultAmountMinor: resultAmountMinor,
		BalanceBeforeMinor: balanceBeforeMinor, BalanceAfterMinor: balanceAfterMinor,
		WalletVersionAfter: walletVersionAfter,
		ReceivedVia:        domain.ReceivedVia(channel), CorrelationID: correlationID,
		PendingAttempts: pendingAttempts, NextRetryAt: nextRetryAt,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	})
}

func minorOrNull(m *domain.Money) any {
	if m == nil {
		return nil
	}
	return m.AmountMinor()
}

// directionOrNull maps the domain's Direction to the column's domain: the DB
// stores a direction only for movements; NONE (no movement) and the zero
// value are persisted as NULL.
func directionOrNull(d domain.Direction) any {
	switch d {
	case domain.DirectionDebit, domain.DirectionCredit:
		return string(d)
	default:
		return nil
	}
}
