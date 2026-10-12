package postgres

import (
	"context"
	"errors"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chalkan33/wallet/internal/app"
	"github.com/chalkan33/wallet/internal/domain"
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

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var id, playerID uuid.UUID
	var currency string
	var balanceMinor, version int64
	var createdAt, updatedAt time.Time
	if err := row.Scan(&id, &playerID, &currency, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrRepoNotFound
		}
		return nil, err
	}
	return domain.RehydrateWallet(id, playerID, currency, balanceMinor, version, createdAt, updatedAt)
}
