package postgres

import (
	"context"
	"errors"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

const walletCols = `id, player_id, currency, balance_minor, version, created_at, updated_at`

type walletRepo struct{ ex querier }

func (r *walletRepo) Create(ctx context.Context, w *domain.Wallet) error {
	_, err := r.ex.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency(), w.Balance().AmountMinor(), w.Version(),
		w.CreatedAt(), w.UpdatedAt())
	return mapInsertError(err)
}

// UpdateBalance persists the post-movement balance and version. Called with
// the row locked inside the same SQL transaction as the ledger entry.
func (r *walletRepo) UpdateBalance(ctx context.Context, w *domain.Wallet) error {
	_, err := r.ex.Exec(ctx, `
		UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		WHERE id = $1`,
		w.ID(), w.Balance().AmountMinor(), w.Version(), w.UpdatedAt())
	return err
}

func (r *walletRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	row := r.ex.QueryRow(ctx, `SELECT `+walletCols+` FROM wallets WHERE id = $1`, id)
	return scanWallet(row)
}

func (r *walletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	row := r.ex.QueryRow(ctx, `SELECT `+walletCols+` FROM wallets WHERE id = $1 FOR UPDATE`, id)
	return scanWallet(row)
}

func (r *walletRepo) GetByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency string, forUpdate bool) (*domain.Wallet, error) {
	q := `SELECT ` + walletCols + ` FROM wallets WHERE player_id = $1 AND currency = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanWallet(r.ex.QueryRow(ctx, q, playerID, currency))
}

// ListIDs pages wallet ids by primary key (id ASC) with a keyset cursor,
// never OFFSET. The sweep that drives continuous reconciliation uses it to
// walk the whole table in bounded batches: ordering by id is a total order
// that survives concurrent inserts and deletes, so no wallet is skipped or
// visited twice across the pages of one pass.
func (r *walletRepo) ListIDs(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := r.ex.Query(ctx, `
		SELECT id FROM wallets
		WHERE ($1::uuid IS NULL OR id > $1)
		ORDER BY id ASC
		LIMIT $2`,
		after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var id, playerID uuid.UUID
	var currency string
	var balanceMinor, version int64
	var createdAt, updatedAt time.Time
	if err := row.Scan(&id, &playerID, &currency, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, service.ErrRepoNotFound
		}
		return nil, err
	}
	return domain.RehydrateWallet(id, playerID, currency, balanceMinor, version, createdAt, updatedAt)
}
