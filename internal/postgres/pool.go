// Package postgres implements the app ports with pgx and explicit SQL.
// Transactions, locks and constraints stay visible here (challenge section 4).
package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
)

// pgxDatabaseName falls back to the DSN database when the config omits it, so
// the db.namespace attribute is never empty.
func pgxDatabaseName(db string) string {
	if db == "" {
		return "wallet"
	}
	return db
}

// NewPool opens the connection pool. Callers own the lifecycle (Fx closes it
// after the workers have stopped).
//
// The pool-level QueryTracer is the single instrumentation point for SQL: one
// injection covers every repository, so no query call site mentions tracing
// and the whole service reports uniform db.* spans.
func NewPool(ctx context.Context, cfg *config.Config, tp *otel.Provider) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DBDSN)
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 15 * time.Minute
	if tp.Enabled() {
		poolCfg.ConnConfig.Tracer = otel.NewSQLTracer(tp, pgxDatabaseName(poolCfg.ConnConfig.Database))
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
