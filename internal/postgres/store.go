package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Store is the pgx-backed TxManager. The SQL transaction boundary is explicit:
// WithinTx commits domain rows, ledger, inbox and outbox atomically.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Repos returns repositories bound to the pool (autocommit reads).
func (s *Store) Repos() service.Repos { return &repos{ex: s.pool} }

// WithinTx runs fn inside one SQL transaction. Read Committed is enough:
// correctness comes from row locks (FOR UPDATE), unique indexes and CHECK
// constraints, not from snapshot isolation.
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, r service.Repos) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(ctx, &repos{ex: tx})
	})
}

// querier is satisfied by both pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// repos produces per-executor repositories (pool or tx bound).
type repos struct {
	ex querier
}

func (r *repos) Wallets() service.WalletRepository           { return &walletRepo{ex: r.ex} }
func (r *repos) WalletOpens() service.WalletOpenRepository   { return &walletOpenRepo{ex: r.ex} }
func (r *repos) Transactions() service.TransactionRepository { return &txRepo{ex: r.ex} }
func (r *repos) Ledger() service.LedgerRepository            { return &ledgerRepo{ex: r.ex} }
func (r *repos) Inbox() service.InboxRepository              { return &inboxRepo{ex: r.ex} }
func (r *repos) Outbox() service.OutboxRepository            { return &outboxRepo{ex: r.ex} }
