package otel

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sql       string
		wantOp    string
		wantTable string
	}{
		{"insert into", `INSERT INTO wager_transactions (id) VALUES ($1)`, "INSERT", "wager_transactions"},
		{"lowercase insert", `insert into wallets (id) values ($1)`, "INSERT", "wallets"},
		{"select with from", `SELECT id, balance FROM wallets WHERE id = $1`, "SELECT", "wallets"},
		{"select from schema-qualified", `SELECT * FROM public.ledger_entries`, "SELECT", "ledger_entries"},
		{"select from quoted table", `SELECT * FROM "outbox" WHERE published_at IS NULL`, "SELECT", "outbox"},
		{"update table set", `UPDATE wallets SET balance = $2 WHERE id = $1`, "UPDATE", "wallets"},
		{"delete from", `DELETE FROM inbox WHERE id = $1`, "DELETE", "inbox"},
		{"leading line comment is skipped", `-- claim due rows
SELECT id FROM wager_transactions`, "SELECT", "wager_transactions"}, {"leading block comment is skipped", "/* relay */ UPDATE outbox SET published_at = $1", "UPDATE", "outbox"},
		{"stacked comments", "-- a\n/* b */\nDELETE FROM inbox", "DELETE", "inbox"},
		{"insert without into keyword", `INSERT wallets (id) VALUES ($1)`, "INSERT", ""},
		{"cte select", `WITH claimed AS (SELECT * FROM outbox) SELECT * FROM claimed`, "WITH", ""},
		{"for update skip locked", "SELECT id FROM outbox FOR UPDATE SKIP LOCKED", "SELECT", "outbox"},
		{"trailing semicolon is stripped", `SELECT 1 FROM wallets;`, "SELECT", "wallets"},
		{"empty statement", ``, "", ""},
		{"only whitespace", "   \n\t ", "", ""},
		{"only a comment", `-- nothing here`, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, table := classify(tc.sql)
			assert.Equal(t, tc.wantOp, op, "operation")
			assert.Equal(t, tc.wantTable, table, "table")
		})
	}
}

// TestClassifyProducesBoundedLabels guards the cardinality contract: span
// names feed dashboards, so an unbounded name (a table interpolated from
// data, a whole statement) would blow up the series count.
func TestClassifyProducesBoundedLabels(t *testing.T) {
	t.Parallel()
	op, table := classify(`INSERT INTO wager_transactions (id, provider_id) VALUES ($1, $2)`)
	require.Equal(t, "INSERT", op)
	require.Equal(t, "wager_transactions", table)
	assert.NotContains(t, table, " ", "table label must be a single identifier")
}

func TestCleanIdent(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{`wallets`, "wallets"},
		{`"wallets"`, "wallets"},
		{`public.wallets`, "wallets"},
		{`wallets,`, "wallets"},
		{`wallets;`, "wallets"},
		{`(wallets)`, "wallets"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, cleanIdent(tc.in))
		})
	}
}

func TestClampRatio(t *testing.T) {
	t.Parallel()
	// A zero/negative ratio is treated as "unset" and samples everything;
	// silently dropping every span would make tracing useless without saying
	// so, which is the failure mode this function exists to prevent.
	assert.Equal(t, 1.0, clampRatio(0))
	assert.Equal(t, 1.0, clampRatio(-1))
	assert.Equal(t, 1.0, clampRatio(1))
	assert.Equal(t, 1.0, clampRatio(2))
	assert.Equal(t, 0.25, clampRatio(0.25))
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, Config{Enabled: false}.Validate(), "disabled tracing needs no endpoint")
	require.ErrorIs(t, Config{Enabled: true}.Validate(), ErrNoEndpoint)
	require.NoError(t, Config{Enabled: true, Endpoint: "collector:4317"}.Validate())
}

// TestDisabledProviderIsUsable pins the nil-safety contract: instrumentation
// runs unconditionally in production code, so a disabled or absent provider
// must still hand out a working tracer.
func TestDisabledProviderIsUsable(t *testing.T) {
	t.Parallel()
	p, err := New(t.Context(), Config{Enabled: false})
	require.NoError(t, err)
	require.False(t, p.Enabled())
	assert.NotNil(t, p.Tracer(), "tracer must never be nil")
	assert.NoError(t, p.Shutdown(t.Context()))
}

func TestNilProviderIsUsable(t *testing.T) {
	t.Parallel()
	var p *Provider
	assert.False(t, p.Enabled())
	assert.NotNil(t, p.Tracer())
	assert.NoError(t, p.Shutdown(t.Context()))
}

// TestNewEnabledBuildsResource is the regression test for a startup abort that
// the disabled-by-default flag hid: building the resource merged the service
// attributes with sdkresource.Default(), whose Schema URL comes from the SDK's
// own semconv. When the version imported here disagreed with the SDK's, Merge
// failed with ErrSchemaURLConflict and the process crash-looped the moment
// tracing was switched on. Exercising the enabled path is therefore the point
// of this test, not the attributes themselves.
func TestNewEnabledBuildsResource(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Enabled:       true,
		Endpoint:      "127.0.0.1:4317",
		Insecure:      true,
		ServiceName:   "wallet-test",
		Environment:   "test",
		InstanceID:    "wallet-test-1",
		SampleRatio:   1.0,
		ExportTimeout: time.Second,
		BatchTimeout:  time.Second,
		MaxQueueSize:  16,
	}
	p, err := New(t.Context(), cfg)
	require.NoError(t, err, "an enabled provider must build without a Collector running")
	t.Cleanup(func() { assert.NoError(t, p.Shutdown(context.Background())) })

	require.True(t, p.Enabled())
	require.NotNil(t, p.TracerProvider())

	attrs := attribute.NewSet(p.resource.Attributes()...)
	for _, want := range []string{
		string(semconv.ServiceNameKey),
		string(semconv.ServiceVersionKey),
		string(semconv.DeploymentEnvironmentNameKey),
		string(semconv.ServiceInstanceIDKey),
	} {
		_, ok := attrs.Value(attribute.Key(want))
		assert.Truef(t, ok, "resource is missing %s", want)
	}
}

// TestIdentifiedProviderKeepsInstanceID guards the replica-discriminating
// attribute: without it, three instances collapse into one line in the trace
// UI and the multi-instance concurrency story is untraceable.
func TestIdentifiedProviderKeepsInstanceID(t *testing.T) {
	t.Parallel()
	p, err := New(t.Context(), Config{
		Enabled: true, Endpoint: "127.0.0.1:4317", Insecure: true,
		ServiceName: "wallet", InstanceID: "wallet-3",
		ExportTimeout: time.Second, BatchTimeout: time.Second, MaxQueueSize: 16,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, p.Shutdown(context.Background())) })

	set := attribute.NewSet(p.resource.Attributes()...)
	v, ok := set.Value(semconv.ServiceInstanceIDKey)
	require.True(t, ok)
	assert.Equal(t, "wallet-3", v.AsString())
}

// TestRecordOutcomeMarksFailuresAsErrors pins the mapping that the error-rate
// panels depend on. Setting only the wallet.status attribute left every
// rejected transaction on a STATUS_CODE_UNSET span, so the traces claimed
// success while the attributes told the opposite story.
func TestRecordOutcomeMarksFailuresAsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      string
		failure     string
		wantIsError bool
	}{
		{"processed is not an error", "PROCESSED", "", false},
		{"rejected is an error", StatusRejected, "INSUFFICIENT_FUNDS", true},
		{"failed is an error", StatusFailed, "INTERNAL_ERROR", true},
		{"rejected without code still errors", StatusRejected, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

			_, span := tp.Tracer("test").Start(t.Context(), "op")
			RecordOutcome(span, tc.status, tc.failure)
			span.End()

			ended := sr.Ended()
			require.Len(t, ended, 1)
			got := ended[0].Status().Code == codes.Error
			assert.Equalf(t, tc.wantIsError, got,
				"status %q with code %q: span status code = %v, attrs = %v",
				tc.status, tc.failure, ended[0].Status().Code, ended[0].Attributes())
		})
	}
}

// TestFailureStatusesMatchDomainStatuses guards the duplicated vocabulary:
// the strings here must stay equal to domain.TxStatus values, or the panel
// filter silently stops matching.
func TestFailureStatusesMatchDomainStatuses(t *testing.T) {
	t.Parallel()
	assert.Equal(t, string(domain.StatusRejected), StatusRejected)
	assert.Equal(t, string(domain.StatusFailed), StatusFailed)
	assert.True(t, isFailureStatus(StatusRejected))
	assert.True(t, isFailureStatus(StatusFailed))
	assert.False(t, isFailureStatus(string(domain.StatusProcessed)))
	assert.False(t, isFailureStatus(string(domain.StatusPending)))
}

// TestTelemetryReportsEffectiveConfig pins that the provider hands back the
// configuration it was actually built with — the startup log line describes
// what is running, not what was requested, so a typo in the effective config
// must be visible. The nil provider returns a zero Config instead of
// panicking, because registration runs unconditionally.
func TestTelemetryReportsEffectiveConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{Enabled: true, Endpoint: "collector:4317", ServiceName: "wallet", InstanceID: "wallet-1"}
	p, err := New(t.Context(), Config{Enabled: false})
	require.NoError(t, err)
	assert.Equal(t, Config{Enabled: false}, p.Telemetry(),
		"an effective config is reported verbatim")

	// A zero provider (nil) must not dereference.
	var nilP *Provider
	assert.Equal(t, Config{}, nilP.Telemetry())

	// And a real enabled provider reports exactly what it was given.
	enabled, err := New(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enabled.Shutdown(context.Background()) })
	assert.Equal(t, cfg, enabled.Telemetry())
}

// TestLogAttrsExposeTelemetryIdentity pins the shape of the startup log line's
// fields. They are consumed by operators and by log-based alerting, so the
// keys (and their order) are part of the contract, not an implementation
// detail.
func TestLogAttrsExposeTelemetryIdentity(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Enabled: true, Endpoint: "collector:4317",
		ServiceName: "wallet", InstanceID: "wallet-7", SampleRatio: 0.25,
	}
	attrs := cfg.LogAttrs()
	require.Len(t, attrs, 5)

	keys := make([]string, 0, len(attrs))
	for _, a := range attrs {
		kv, ok := a.(slog.Attr)
		require.True(t, ok, "every attribute must be a slog.Attr")
		keys = append(keys, kv.Key)
	}
	assert.Equal(t, []string{"enabled", "endpoint", "service", "instance", "sampleRatio"}, keys)
}
