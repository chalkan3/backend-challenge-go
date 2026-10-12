package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/chalkan3/backend-challenge-go/internal/migrations"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// MigrateUp applies every pending migration (idempotent).
func MigrateUp(ctx context.Context, dsn string) error {
	p, db, err := openGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = p.Up(ctx)
	return err
}

// MigrateDown rolls back ONE migration step (repeat to go further). Documented
// in README; reversal is intentionally manual and auditable.
func MigrateDown(ctx context.Context, dsn string) error {
	p, db, err := openGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = p.Down(ctx)
	return err
}

// MigrateStatus reports applied versions.
func MigrateStatus(ctx context.Context, dsn string) error {
	p, db, err := openGoose(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		state := "pending"
		if r.State == goose.StateApplied {
			state = "applied"
		}
		path := ""
		if r.Source != nil {
			path = r.Source.Path
		}
		fmt.Printf("%-8s %s\n", state, path)
	}
	return nil
}

func openGoose(dsn string) (*goose.Provider, *sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("migrations: database unreachable: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithVerbose(false))
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return p, db, nil
}

// --- shared error mapping ---------------------------------------------------

// mapInsertError translates a unique violation on identity indexes into
// ErrDuplicateInsert so the use case can replay the concurrent winner.
func mapInsertError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return service.ErrDuplicateInsert
	}
	return err
}
