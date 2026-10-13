package otel

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The SQL tracer is the single injection point that instruments every
// repository call, and it is installed on the pool at bootstrap. These tests
// drive the QueryTracer hooks directly against a recording tracer provider so
// the span name, attributes and error mapping are pinned without a database.

func newRecordingTracer(t *testing.T) (*SQLTracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return &SQLTracer{tracer: tp.Tracer("test"), dbName: "wallet"}, sr
}

func TestSQLTracerNamesSpanFromStatement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sql       string
		wantName  string
		wantTable string
	}{
		{"select", "SELECT id FROM wallets WHERE id = $1", "sql select wallets", "wallets"},
		{"insert", "INSERT INTO wager_transactions (id) VALUES ($1)", "sql insert wager_transactions", "wager_transactions"},
		{"update", "UPDATE wallets SET balance_minor = $1", "sql update wallets", "wallets"},
		{"delete", "DELETE FROM consumer_inbox WHERE completed_at IS NULL", "sql delete consumer_inbox", "consumer_inbox"},
		{"no table", "BEGIN", "sql begin", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, sr := newRecordingTracer(t)

			ctx := tr.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: tc.sql})
			require.NotNil(t, ctx)
			// Close it so the span is recorded.
			tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})

			ended := sr.Ended()
			require.Len(t, ended, 1)
			span := ended[0]
			assert.Equal(t, tc.wantName, span.Name())

			attrs := map[string]any{}
			for _, kv := range span.Attributes() {
				attrs[string(kv.Key)] = kv.Value.AsInterface()
			}
			assert.Equal(t, "postgresql", attrs[string(AttrDBSystem)])
			assert.Equal(t, "wallet", attrs[string(AttrDBName)])
			if tc.wantTable != "" {
				assert.Equal(t, tc.wantTable, attrs[string(AttrDBTable)])
			} else {
				assert.NotContains(t, attrs, string(AttrDBTable))
			}
		})
	}
}

func TestSQLTracerRecordsRowsAffectedOnSuccess(t *testing.T) {
	t.Parallel()
	tr, sr := newRecordingTracer(t)

	ctx := tr.TraceQueryStart(t.Context(), nil,
		pgx.TraceQueryStartData{SQL: "UPDATE wallets SET version = version + 1"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag("UPDATE 3"),
	})

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Unset, ended[0].Status().Code, "a successful statement must not mark the span as failed")

	var rows int64 = -1
	for _, kv := range ended[0].Attributes() {
		if string(kv.Key) == string(AttrRowsAffected) {
			rows = kv.Value.AsInt64()
		}
	}
	assert.Equal(t, int64(3), rows, "rows affected must be reported")
}

func TestSQLTracerRecordsErrorOnFailure(t *testing.T) {
	t.Parallel()
	tr, sr := newRecordingTracer(t)
	cause := errors.New("deadlock detected")

	ctx := tr.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1 FROM wallets"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: cause})

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, cause.Error(), ended[0].Status().Description)
	require.NotEmpty(t, ended[0].Events(), "the error must be recorded as a span event")
}

// TestNewSQLTracerWiresProviderAndName covers the constructor against a
// disabled provider (the local/CI default) — it must still be usable.
func TestNewSQLTracerWiresProviderAndName(t *testing.T) {
	t.Parallel()
	var p *Provider // nil-safe by contract
	tr := NewSQLTracer(p, "wallet")
	require.NotNil(t, tr)
	assert.Equal(t, "wallet", tr.dbName)
	assert.NotNil(t, tr.tracer)

	// A statement still flows through without panicking when tracing is off.
	ctx := tr.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
}
