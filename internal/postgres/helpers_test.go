package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
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
