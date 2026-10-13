package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Unit tests for the pure helpers of the pgx adapter. The SQL itself is
// verified against a real PostgreSQL in test/integration — the challenge
// forbids replacing the database with mocks — but the adapter's pure
// translations (error mapping, NULL encoding) are unit-testable in isolation
// and are pinned here so a refactor cannot silently change their contract.

func TestMapInsertError_UniqueViolationBecomesDuplicateInsert(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	err := mapInsertError(pgErr)

	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrDuplicateInsert,
		"a 23505 unique violation must surface as ErrDuplicateInsert for idempotent replay")
}

func TestMapInsertError_OtherPgErrorsPassThrough(t *testing.T) {
	other := &pgconn.PgError{Code: "23514", Message: "check violation"}
	assert.Equal(t, error(other), mapInsertError(other),
		"non-unique PG errors must not be rewritten")
}

func TestMapInsertError_NilStaysNil(t *testing.T) {
	assert.NoError(t, mapInsertError(nil))
}

func TestMapInsertError_WrappedUniqueViolationIsDetected(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	wrapped := fmt.Errorf("tx: %w", pgErr)
	assert.ErrorIs(t, mapInsertError(wrapped), service.ErrDuplicateInsert,
		"errors.As must see through wrapping")
}

func TestMapInsertError_PlainErrorPassesThrough(t *testing.T) {
	sentinel := errors.New("boom")
	assert.Equal(t, sentinel, mapInsertError(sentinel))
}

func TestMinorOrNull(t *testing.T) {
	m := domain.ZeroMoneyMust("BRL")
	assert.Nil(t, minorOrNull(nil), "nil Money encodes as SQL NULL")
	assert.Equal(t, int64(0), minorOrNull(&m))

	amt, err := domain.ParseMoney("42.50", "BRL")
	require.NoError(t, err)
	assert.Equal(t, int64(4250), minorOrNull(&amt))
}

func TestDirectionOrNull(t *testing.T) {
	assert.Nil(t, directionOrNull(domain.DirectionNone), "no movement encodes as SQL NULL")
	assert.Nil(t, directionOrNull(domain.Direction("")), "zero value encodes as SQL NULL")
	assert.Equal(t, "DEBIT", directionOrNull(domain.DirectionDebit))
	assert.Equal(t, "CREDIT", directionOrNull(domain.DirectionCredit))
}

// TestPgxDatabaseName pins the db.namespace fallback: a config that omits the
// database name must still yield a meaningful attribute instead of an empty
// string in every SQL span.
func TestPgxDatabaseName(t *testing.T) {
	assert.Equal(t, "wallet", pgxDatabaseName(""))
	assert.Equal(t, "custom_db", pgxDatabaseName("custom_db"))
}

// TestNewPoolRejectsMalformedDSN pins the fail-fast contract of pool
// construction: a DSN pgx cannot parse must be rejected before any connection
// is attempted, so the process dies at boot rather than on the first query.
func TestNewPoolRejectsMalformedDSN(t *testing.T) {
	ctx := context.Background()
	tp, err := otel.New(ctx, otel.Config{Enabled: false})
	require.NoError(t, err)

	// A DSN with a scheme pgx does not recognise fails ParseConfig.
	_, err = NewPool(ctx, &config.Config{DBDSN: "not-a-dsn://:::"}, tp)
	require.Error(t, err)
}

// TestNewPoolFailsOnUnreachableDatabase pins the second fail-fast gate: even a
// well-formed DSN whose server is unreachable must fail Ping and close the
// pool, never return a half-open pool that would fail later on the first
// repository call (where the error would be far from its cause).
func TestNewPoolFailsOnUnreachableDatabase(t *testing.T) {
	ctx := context.Background()
	tp, err := otel.New(ctx, otel.Config{Enabled: false})
	require.NoError(t, err)

	// Port 1 is reserved and never listening; the Ping will fail fast.
	_, err = NewPool(ctx, &config.Config{
		DBDSN:      "postgres://wallet:wallet@127.0.0.1:1/wallet?sslmode=disable",
		DBMaxConns: 1,
	}, tp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect", "the failure must be the connect/ping error")
}

// TestOpenGooseFailsOnUnreachableDatabase pins openGoose's fail-fast: an
// unreachable database must be reported as such (with the migrations context
// wrapped in), not as a nil-pointer later during Up/Down.
func TestOpenGooseFailsOnUnreachableDatabase(t *testing.T) {
	_, _, err := openGoose("postgres://wallet:wallet@127.0.0.1:1/wallet?sslmode=disable")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migrations",
		"the error must name the migration subsystem for a fast diagnosis")
}

// TestMigrateUpFailsOnUnreachableDatabase confirms the whole entry point, not
// just its helper, surfaces the failure instead of succeeding silently.
func TestMigrateUpFailsOnUnreachableDatabase(t *testing.T) {
	err := MigrateUp(context.Background(), "postgres://wallet:wallet@127.0.0.1:1/wallet?sslmode=disable")
	require.Error(t, err)
}
