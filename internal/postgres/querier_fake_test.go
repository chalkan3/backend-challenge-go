package postgres

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeQuerier is an in-memory satisfier of the production `querier` interface
// (Exec/Query/QueryRow/SendBatch), which is the single seam every repository in
// this package is built on.
//
// It exists to unit-test the adapter's *translations* — pgx error mapping, NULL
// encoding, row scanning, keyset cursor construction — without a database.
// It deliberately does NOT pretend to be PostgreSQL: there is no SQL parsing
// and no transaction semantics here. The SQL itself, the row locks and the
// constraints are proven against a real PostgreSQL under the `integration`
// build tag (test/integration, internal/postgres/repo_integration_test.go).
//
// Keeping the two layers separate is the point: a fake can pin "a 23505 becomes
// ErrDuplicateInsert" but it can never prove "two concurrent BETs cannot both
// win the same row".
type fakeQuerier struct {
	mu sync.Mutex

	// Recorded calls, for assertions on the SQL and the arguments.
	execCalls     []execCall
	queryRowCalls []queryCall
	queryCalls    []queryCall
	sendBatchCall int

	// Canned responses.
	execTag   pgconn.CommandTag
	execErr   error
	row       pgx.Row
	queryRows pgx.Rows
	queryErr  error

	// Batch state: how many statements were queued, and an optional error
	// returned from the batch's Close.
	queuedInBatch int
	batchErr      error
}

type execCall struct {
	sql  string
	args []any
}

type queryCall struct {
	sql  string
	args []any
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, execCall{sql: sql, args: args})
	return f.execTag, f.execErr
}

func (f *fakeQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryCalls = append(f.queryCalls, queryCall{sql: sql, args: args})
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.queryRows, nil
}

func (f *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queryRowCalls = append(f.queryRowCalls, queryCall{sql: sql, args: args})
	return f.row
}

func (f *fakeQuerier) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendBatchCall++
	if batch != nil {
		f.queuedInBatch = batch.Len()
	}
	if f.batchErr != nil {
		return &stubBatchResults{err: f.batchErr}
	}
	return &stubBatchResults{}
}

// lastExec returns the most recent Exec call, failing the test if there was none.
func (f *fakeQuerier) lastExec(t interface{ Fatalf(string, ...any) }) execCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execCalls) == 0 {
		t.Fatalf("expected an Exec call, got none")
	}
	return f.execCalls[len(f.execCalls)-1]
}

// lastQueryRow returns the most recent QueryRow call.
func (f *fakeQuerier) lastQueryRow(t interface{ Fatalf(string, ...any) }) queryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queryRowCalls) == 0 {
		t.Fatalf("expected a QueryRow call, got none")
	}
	return f.queryRowCalls[len(f.queryRowCalls)-1]
}

// ── canned rows ─────────────────────────────────────────────────────────────

// stubRow is a pgx.Row that yields fixed values, or a fixed error.
type stubRow struct {
	values []any
	err    error
}

func (r stubRow) Scan(dest ...any) error { return assign(r.values, r.err, dest...) }

// errRow always fails on Scan, which is how the scanner tests exercise the
// pgx.ErrNoRows → service.ErrRepoNotFound translation.
type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

// assign copies canned values into the scan destinations, mimicking pgx's
// pointer-based contract closely enough for the adapter's scanning code.
func assign(values []any, err error, dest ...any) error {
	if err != nil {
		return err
	}
	if len(values) != len(dest) {
		return errAssignArity
	}
	for i := range dest {
		if err := assignOne(dest[i], values[i]); err != nil {
			return err
		}
	}
	return nil
}

// assignOne mirrors pgx's pointer-based scan contract for the concrete types
// this package scans into: uuid.UUID, string, time.Time, the int64/string
// aliases used by the domain, and pointers to any of those (NULLable columns).
func assignOne(dest, value any) error {
	if value == nil {
		return assignNull(dest)
	}
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return &assignError{"scan destination must be a non-nil pointer"}
	}
	target := rv.Elem()
	return assignReflect(target, reflect.ValueOf(value))
}

// assignNull handles a NULL column: it zeroes the destination if it is a
// pointer (setting it to nil) and errors if a non-pointer destination is asked
// to hold NULL — which is exactly the failure mode pgx reports.
func assignNull(dest any) error {
	target := reflect.ValueOf(dest).Elem()
	if target.Kind() == reflect.Pointer {
		target.SetZero()
		return nil
	}
	return &assignError{"cannot scan NULL into a non-pointer destination"}
}

func assignReflect(target, src reflect.Value) error {
	// Assignable (e.g. string column → domain.Direction, whose underlying type
	// is string) still needs an explicit Convert when the types differ.
	if src.Type().AssignableTo(target.Type()) {
		target.Set(src)
		return nil
	}
	if src.Type().ConvertibleTo(target.Type()) {
		target.Set(src.Convert(target.Type()))
		return nil
	}
	// string ↔ []byte: pgx hands back a `text` column as a string and a `jsonb`
	// column as []byte, while the destination may be either. The conversion is
	// explicit here so it is visible, not implied by the fake.
	if src.Kind() == reflect.String && target.Kind() == reflect.Slice &&
		target.Type().Elem().Kind() == reflect.Uint8 {
		target.SetBytes([]byte(src.String()))
		return nil
	}
	if src.Kind() == reflect.Slice && src.Type().Elem().Kind() == reflect.Uint8 &&
		target.Kind() == reflect.String {
		target.SetString(string(src.Bytes()))
		return nil
	}
	// Value needs to land in a pointer destination (non-NULL nullable column).
	if target.Kind() == reflect.Pointer {
		ptr := reflect.New(target.Type().Elem())
		if err := assignReflect(ptr.Elem(), src); err != nil {
			return err
		}
		target.Set(ptr)
		return nil
	}
	// A pointer value being scanned into a non-pointer destination.
	if src.Kind() == reflect.Pointer {
		if src.IsNil() {
			return assignNull(target.Addr().Interface())
		}
		return assignReflect(target, src.Elem())
	}
	return &assignError{fmt.Sprintf("incompatible scan types: %s → %s", src.Type(), target.Type())}
}

var errAssignArity = &assignError{"stub row: value count does not match scan destinations"}

type assignError struct{ msg string }

func (e *assignError) Error() string { return e.msg }

// stubBatchResults satisfies pgx.BatchResults. Close is the call the
// repositories make to flush the batch, so it is the one that reports errors.
type stubBatchResults struct {
	pgx.BatchResults
	err error
}

func (b *stubBatchResults) Close() error { return b.err }

// ── canned result sets ──────────────────────────────────────────────────────

// stubRows is a pgx.Rows over a fixed set of canned rows. It embeds the pgx
// interface so the methods the repositories never call (Values, RawValues,
// Conn, TypeMap, CommandTag, FieldDescriptions) need no stub body: calling one
// would panic on the nil embedded interface, which is the loud failure we want
// if someone starts depending on it.
type stubRows struct {
	pgx.Rows
	rows   [][]any
	idx    int
	err    error
	closed bool
	// scanFailAt makes the Nth Scan return scanErr, to exercise the error path
	// inside the repository loops.
	scanFailAt int
	scanErr    error
}

func newStubRows(rows ...[]any) *stubRows { return &stubRows{rows: rows, scanFailAt: -1} }

func (r *stubRows) Close()     { r.closed = true }
func (r *stubRows) Err() error { return r.err }
func (r *stubRows) Next() bool {
	if r.idx >= len(r.rows) {
		r.closed = true
		return false
	}
	r.idx++
	return true
}

func (r *stubRows) Scan(dest ...any) error {
	i := r.idx - 1
	if i == r.scanFailAt {
		return r.scanErr
	}
	if i < 0 || i >= len(r.rows) {
		return &assignError{"Scan called before a successful Next"}
	}
	return assign(r.rows[i], nil, dest...)
}
