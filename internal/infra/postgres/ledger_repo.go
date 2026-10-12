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

type ledgerRepo struct{ ex querier }

func (r *ledgerRepo) Append(ctx context.Context, e domain.LedgerEntry) error {
	_, err := r.ex.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, currency, amount_minor,
			 balance_before_minor, balance_after_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID(), e.WalletID(), e.TransactionID(), e.Direction(), e.Currency(),
		e.Amount().AmountMinor(), e.BalanceBefore().AmountMinor(), e.BalanceAfter().AmountMinor(),
		e.CreatedAt())
	return err
}

// ListByWallet paginates with a stable keyset: (created_at, id) ascending.
// The cursor is opaque to callers (transport encodes/decodes base64 JSON).
func (r *ledgerRepo) ListByWallet(ctx context.Context, walletID uuid.UUID, cursor *app.LedgerCursor, limit int) ([]*domain.LedgerEntry, *app.LedgerCursor, error) {
	fetch := limit + 1
	var curTime *time.Time
	var curID *uuid.UUID
	if cursor != nil {
		t := cursor.CreatedAt
		i := cursor.ID
		curTime, curID = &t, &i
	}
	rows, err := r.ex.Query(ctx, `
		SELECT id, wallet_id, transaction_id, direction, currency, amount_minor,
		       balance_before_minor, balance_after_minor, created_at
		FROM wallet_ledger_entries
		WHERE wallet_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) > ($2, $3::uuid))
		ORDER BY created_at ASC, id ASC
		LIMIT $4`,
		walletID, curTime, curID, fetch)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var out []*domain.LedgerEntry
	for rows.Next() {
		var id, wID, tID uuid.UUID
		var direction, currency string
		var amount, before, after int64
		var createdAt time.Time
		if err := rows.Scan(&id, &wID, &tID, &direction, &currency, &amount, &before, &after, &createdAt); err != nil {
			return nil, nil, err
		}
		amt, err := domain.NewMoney(amount, currency)
		if err != nil {
			return nil, nil, err
		}
		bf, err := domain.NewMoney(before, currency)
		if err != nil {
			return nil, nil, err
		}
		af, err := domain.NewMoney(after, currency)
		if err != nil {
			return nil, nil, err
		}
		entry, err := domain.RehydrateLedgerEntry(domain.LedgerEntryParams{
			ID: id, WalletID: wID, TransactionID: tID,
			Direction: domain.Direction(direction), Amount: amt,
			BalanceBefore: bf, BalanceAfter: af, CreatedAt: createdAt,
		})
		if err != nil {
			return nil, nil, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *app.LedgerCursor
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = &app.LedgerCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
	}
	return out, next, nil
}

// Reconcile computes stored balance, ledger sum and entry count in ONE
// statement — a single consistent snapshot without explicit locks.
func (r *ledgerRepo) Reconcile(ctx context.Context, walletID uuid.UUID) (stored, sum, count int64, err error) {
	err = r.ex.QueryRow(ctx, `
		SELECT w.balance_minor,
		       COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0),
		       COUNT(l.id)
		FROM wallets w
		LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		WHERE w.id = $1
		GROUP BY w.balance_minor`, walletID).Scan(&stored, &sum, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, app.ErrRepoNotFound
	}
	return stored, sum, count, err
}
