// The lifecycle registrars decide what actually starts and stops in
// production: migrations, the HTTP listener, the worker fleet. Most of their
// bodies only run at boot and at shutdown, far from any request, so a bug here
// hides until an operator restarts a live process. These tests drive each
// registrar with the smallest inputs that reach its branches.
package bootstrap

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// TestRegisterMigrationsSkipsInWorkersMode pins the guard that keeps N
// consumer replicas from racing each other on the goose table at startup.
func TestRegisterMigrationsSkipsInWorkersMode(t *testing.T) {
	lc := &capturingLifecycle{}
	registerMigrations(lc, &config.Config{
		MigrateOnStart: true,
		Mode:           config.ModeWorkers,
	}, slog.Default())

	assert.Empty(t, lc.hooks, "a workers-only process must not register a migration hook")
}

// TestRegisterMigrationsSkipsWhenDisabled pins the other guard: with
// MigrateOnStart off, no hook is registered regardless of mode.
func TestRegisterMigrationsSkipsWhenDisabled(t *testing.T) {
	lc := &capturingLifecycle{}
	registerMigrations(lc, &config.Config{
		MigrateOnStart: false,
		Mode:           config.ModeAll,
	}, slog.Default())

	assert.Empty(t, lc.hooks)
}

// TestRegisterMigrationsRegistersHookWhenEnabled pins the positive case: in
// all-mode with MigrateOnStart on, exactly one hook is registered, and running
// its OnStart against an unreachable database surfaces the failure (the hook
// must not swallow it — Fx would otherwise boot a process with no schema).
func TestRegisterMigrationsRegistersHookWhenEnabled(t *testing.T) {
	lc := &capturingLifecycle{}
	registerMigrations(lc, &config.Config{
		MigrateOnStart: true,
		Mode:           config.ModeAll,
		DBDSN:          "postgres://wallet:wallet@127.0.0.1:1/wallet?sslmode=disable",
	}, slog.Default())

	require.Len(t, lc.hooks, 1)
	require.NotNil(t, lc.hooks[0].OnStart)
	assert.Error(t, lc.hooks[0].OnStart(context.Background()),
		"an unreachable database during migration must fail the start hook")
	require.NotNil(t, lc.hooks[0].OnStop)
	assert.NoError(t, lc.hooks[0].OnStop(context.Background()))
}

// TestRegisterWorkersSkipsByModeAndConfig pins the two early-return branches:
// API mode and a config with workers disabled must register no hook. The
// arguments are nil because neither branch ever touches them.
func TestRegisterWorkersSkipsByModeAndConfig(t *testing.T) {
	t.Run("api mode", func(t *testing.T) {
		lc := &capturingLifecycle{}
		registerWorkers(lc, &config.Config{Mode: config.ModeAPI}, slog.Default(), nil, nil, nil, nil)
		assert.Empty(t, lc.hooks)
	})

	t.Run("workers disabled by config", func(t *testing.T) {
		lc := &capturingLifecycle{}
		registerWorkers(lc, &config.Config{
			Mode:           config.ModeAll,
			WorkersEnabled: false,
		}, slog.Default(), nil, nil, nil, nil)
		assert.Empty(t, lc.hooks)
	})
}

// TestRegisterHTTPRegistersHook pins the HTTP registrar's hook pair and the
// drain path: OnStop must call Shutdown on the server, which for a server that
// was never started is an immediate success.
func TestRegisterHTTPRegistersHook(t *testing.T) {
	lc := &capturingLifecycle{}
	srv := minimalServer()

	registerHTTP(lc, &config.Config{ShutdownGrace: 1_000_000_000}, slog.Default(), srv)

	require.Len(t, lc.hooks, 1)
	require.NotNil(t, lc.hooks[0].OnStop)
	assert.NoError(t, lc.hooks[0].OnStop(context.Background()))
}

// minimalServer is an http.Server that was never started: Shutdown on it is
// an immediate success, which lets the registrar's drain path run without
// opening a socket.
func minimalServer() *http.Server {
	return &http.Server{Addr: "127.0.0.1:0"}
}
