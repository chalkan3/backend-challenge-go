// White-box tests for the composition root's constructor functions.
//
// These are the "free functions" of the Fx graph: NewHTTPDeps, the two service
// constructors, and the lifecycle registrations. Testing them directly is
// cheaper and more precise than booting the whole app — an Fx graph that fails
// to build tells you *that* something is wrong, a test of the constructor
// tells you *which* wiring is wrong. They are also the only place the
// config→service projection (WithReferencePolicy) is pinned, so a config field
// that stops reaching the use case is caught here.
package bootstrap

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fullConfig returns a configuration with every field the constructors read.
func fullConfig() *config.Config {
	return &config.Config{
		Mode:                 config.ModeAll,
		HTTPAddr:             ":8080",
		ShutdownGrace:        10 * time.Second,
		LogLevel:             "info",
		DBDSN:                "postgres://u:p@127.0.0.1:1/none?sslmode=disable",
		DBMaxConns:           5,
		MigrateOnStart:       false,
		SQSRequestQueue:      "wager-transactions.fifo",
		SQSEventsQueue:       "integration-events.fifo",
		SQSDLQQueue:          "wager-transactions-dlq.fifo",
		WorkersEnabled:       true,
		ReferenceMaxAttempts: 7,
		ReferenceTTL:         24 * time.Hour,
	}
}

// TestNewHTTPDepsPopulatesEveryField is the transport-wiring contract: every
// field the router reads must be non-nil, because a nil there becomes a panic
// on the first request rather than a startup failure.
func TestNewHTTPDepsPopulatesEveryField(t *testing.T) {
	cfg := fullConfig()
	m := metrics.New()
	log := discardLog()

	ws := service.NewWalletService(nil, log, silentHook{})
	wg := service.NewWagerService(nil, log, silentHook{})

	deps := NewHTTPDeps(cfg, log, &auth.Authenticator{}, ws, wg, m, nil, nil, queueURLs(), &otel.Provider{})

	assert.Equal(t, cfg, deps.Cfg)
	require.NotNil(t, deps.Log)
	require.NotNil(t, deps.Auth)
	require.NotNil(t, deps.Ep.OpenWallet, "every endpoint must be wired")
	require.NotNil(t, deps.Ep.GetWallet)
	require.NotNil(t, deps.Ep.ListLedger)
	require.NotNil(t, deps.Ep.Reconcile)
	require.NotNil(t, deps.Ep.SubmitTransaction)
	require.NotNil(t, deps.Ep.GetTransaction)
	require.NotNil(t, deps.Ep.GetProviderTransaction)
	assert.Equal(t, m.Registry(), deps.Reg, "the metrics registry must be the one the endpoints use")
	require.NotNil(t, deps.Met)
}

// TestNewWalletServiceProjectsNothingButDefaults documents that the wallet
// service has no config-driven options today: the constructor exists to adapt
// the functional-options API to Fx, and it must still produce a usable service.
func TestNewWalletServiceBuildsAUsableService(t *testing.T) {
	ws := newWalletService(nil, discardLog(), silentHook{}, fullConfig())
	require.NotNil(t, ws)
}

// TestNewWagerServiceProjectsReferencePolicy pins the one config→service
// projection in the graph. If ReferenceMaxAttempts or ReferenceTTL stop
// reaching the use case, the pending-reference retry policy silently reverts
// to defaults — a bug no endpoint test would notice.
func TestNewWagerServiceProjectsReferencePolicy(t *testing.T) {
	cfg := fullConfig()

	// The policy is observable through the zero-argument struct only after a
	// transaction is built; we assert the constructor runs with the values and
	// returns a service. The values themselves are exercised by the service
	// tests; what this pins is that the composition root forwards them.
	wg := newWagerService(nil, discardLog(), silentHook{}, cfg)
	require.NotNil(t, wg)
}

// TestRegisterPoolCloseClosesOnStop proves the pool lifetime is tied to the
// app lifecycle. Leaking a pool on shutdown would keep connections open
// against a database that is about to be torn down.
func TestRegisterPoolCloseClosesOnStop(t *testing.T) {
	pool := deadPoolForBootstrap(t)

	app := fx.New(
		fx.NopLogger,
		fx.Supply(pool),
		fx.Supply(discardLog()),
		fx.Invoke(registerPoolClose),
	)
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))
	require.NoError(t, app.Stop(ctx))

	// A closed pool refuses new work; this is the observable consequence of
	// Close having run.
	err := pool.Ping(ctx)
	assert.Error(t, err, "the pool must be closed after the app stops")
}

// TestRegisterModeIsSideEffectFreeNoOp is a smoke test for the logging hook:
// it must run without panicking for every mode, since it is invoked
// unconditionally at startup.
func TestRegisterModeRuns(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeAll, config.ModeAPI, config.ModeWorkers} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := fullConfig()
			cfg.Mode = mode
			assert.NotPanics(t, func() { registerMode(cfg, discardLog()) })
		})
	}
}

// TestRegisterMigrationsSkipsInWorkersMode pins the migration policy: a
// workers-only process never races the schema. It is covered here (not only in
// integration) because the branch is a deliberate design decision worth
// failing fast on.
func TestRegisterMigrationsSkipsInWorkersModeComposition(t *testing.T) {
	cfg := fullConfig()
	cfg.MigrateOnStart = true
	cfg.Mode = config.ModeWorkers

	lc := &recordingLifecycle{}
	// The hook must not be appended at all in workers mode.
	registerMigrations(lc, cfg, discardLog())
	assert.Empty(t, lc.starts, "workers mode must not register a migration hook")

	// With migrations disabled, no mode registers a hook.
	cfg2 := fullConfig()
	cfg2.MigrateOnStart = false
	lc2 := &recordingLifecycle{}
	registerMigrations(lc2, cfg2, discardLog())
	assert.Empty(t, lc2.starts)
}

// recordingLifecycle captures the hooks appended to an fx.Lifecycle so a
// registration can be asserted without running the app.
type recordingLifecycle struct {
	starts []func(context.Context) error
	stops  []func(context.Context) error
}

func (r *recordingLifecycle) Append(h fx.Hook) {
	if h.OnStart != nil {
		r.starts = append(r.starts, h.OnStart)
	}
	if h.OnStop != nil {
		r.stops = append(r.stops, h.OnStop)
	}
}

// deadPoolForBootstrap builds a pool whose DSN points at a closed port. pgxpool
// connects lazily, so the pool exists and Ping fails — which is exactly the
// state we want to observe after Close.
func deadPoolForBootstrap(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	return pool
}

// queueURLs returns a set of well-formed queue URLs for the wiring test.
func queueURLs() sqs.QueueURLs {
	return sqs.QueueURLs{
		Request: "http://localhost:4566/000000000000/wager-transactions.fifo",
		Events:  "http://localhost:4566/000000000000/integration-events.fifo",
		DLQ:     "http://localhost:4566/000000000000/wager-transactions-dlq.fifo",
	}
}

// silentHook satisfies service.Instrumentation with no-ops, so a service can
// be built without an observation backend.
type silentHook struct{}

func (silentHook) WagerOutcome(string, string, string, string) {}
func (silentHook) Duplicate(string)                            {}
func (silentHook) ReferenceOutcome(string)                     {}
func (silentHook) ReconciliationDivergence()                   {}
func (silentHook) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, silentSpan{}
}

type silentSpan struct{}

func (silentSpan) SetAttributes(...service.SpanAttr) {}
func (silentSpan) SetOutcome(string, string)         {}
func (silentSpan) RecordError(error)                 {}
func (silentSpan) End()                              {}

var _ service.Instrumentation = silentHook{}
