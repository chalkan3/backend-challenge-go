// sqltracer.go instruments every SQL statement with one pgx QueryTracer,
// installed on the pool. One injection point covers the whole service: the
// hundreds of repository calls never mention tracing, and no repository needs
// a tracer dependency.
//
// It reports a span per statement with the db.* semantic conventions, plus
// the outcome (rows for queries, rows affected for writes). Query text is
// reduced to its operation + table so a span carries a stable, low-cardinality
// name — the same reduction unit tests pin.
//
// Note on latency: because the tracer observes the driver, the duration is
// the real round-trip including the pool wait, which is what the dashboards
// care about.
package otel

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// SQLTracer is a pgx.QueryTracer that starts a span per statement.
type SQLTracer struct {
	tracer trace.Tracer
	dbName string
}

// NewSQLTracer builds the pool-level query tracer.
func NewSQLTracer(p *Provider, dbName string) *SQLTracer {
	return &SQLTracer{tracer: p.Tracer(), dbName: dbName}
}

// TraceQueryStart is part of the pgx.QueryTracer contract.
func (t *SQLTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	op, table := classify(data.SQL)
	name := "sql " + strings.ToLower(op)
	if table != "" {
		name += " " + table
	}
	ctx, _ = t.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
	)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		AttrDBSystem.String("postgresql"),
		AttrDBName.String(t.dbName),
		AttrDBOp.String(op),
	)
	if table != "" {
		span.SetAttributes(AttrDBTable.String(table))
	}
	return ctx
}

// TraceQueryEnd closes the span, recording the driver error when the
// statement failed. The span itself stays the source of the timing.
func (t *SQLTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	defer span.End()
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
		return
	}
	rows := data.CommandTag.RowsAffected()
	span.SetAttributes(AttrRowsAffected.Int64(rows))
}

// classify reduces a statement to (operation, table). Keeping this a pure
// function makes it table-testable without a database.
func classify(sql string) (op, table string) {
	s := strings.TrimSpace(sql)
	// Skip leading SQL comments ("-- update ..." must not be read as UPDATE).
	for {
		s2 := strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s2, "--"):
			if i := strings.IndexByte(s2, '\n'); i >= 0 {
				s = s2[i+1:]
				continue
			}
			return "", ""
		case strings.HasPrefix(s2, "/*"):
			if i := strings.Index(s2, "*/"); i >= 0 {
				s = s2[i+2:]
				continue
			}
			return "", ""
		}
		s = s2
		break
	}

	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", ""
	}
	op = strings.ToUpper(fields[0])

	// INSERT INTO t / UPDATE t / DELETE FROM t / SELECT ... FROM t
	switch op {
	case "INSERT", "DELETE":
		// The table follows the keyword: "INSERT INTO t", "DELETE FROM t".
		for i, f := range fields {
			switch strings.ToUpper(f) {
			case "INTO", "FROM":
				if i+1 < len(fields) {
					return op, cleanIdent(fields[i+1])
				}
			}
		}
	case "UPDATE":
		// "UPDATE t SET ..." — the table follows the verb directly.
		if len(fields) > 1 {
			table = cleanIdent(fields[1])
		}
	case "SELECT":
		for i, f := range fields {
			if strings.EqualFold(f, "FROM") && i+1 < len(fields) {
				return op, cleanIdent(fields[i+1])
			}
		}
	}
	return op, table
}

// cleanIdent strips quoting, schema prefixes and trailing punctuation so
// label values stay bounded.
func cleanIdent(s string) string {
	s = strings.Trim(s, `"',;()`)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(s, `"`)
}
