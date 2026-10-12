//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/chalkan3/backend-challenge-go/internal/bootstrap"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// TestModeGraphBuildsEveryRole is the composition test for the three entry
// points of the single-binary topology (WALLET_MODE=all|api|workers).
//
// It runs under the integration tag because the Fx graph opens real
// dependencies at build time (postgres.NewPool pings the database), so the
// stack from `make e2e` must be up. The value is exact: if a mode's wiring is
// unsatisfiable — a worker asking for something only api mode provides, or a
// route registered against a missing dependency — Fx fails here, at build
// time, instead of at boot in production.
func TestModeGraphBuildsEveryRole(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the compose stack")
	}

	tests := []struct {
		mode          config.Mode
		wantWorkers   bool
		wantHTTPRoute bool
	}{
		{mode: config.ModeAll, wantWorkers: true, wantHTTPRoute: true},
		{mode: config.ModeAPI, wantWorkers: false, wantHTTPRoute: true},
		{mode: config.ModeWorkers, wantWorkers: true, wantHTTPRoute: false},
	}

	for _, tc := range tests {
		t.Run(tc.mode.String(), func(t *testing.T) {
			// Each subtest loads its own config through the same env the app
			// reads. Migrations are off: this test asserts wiring, not schema.
			t.Setenv("WALLET_MODE", tc.mode.String())
			t.Setenv("WALLET_MIGRATE_ON_START", "false")
			t.Setenv("WALLET_HTTP_ADDR", "127.0.0.1:0") // ephemeral, never served

			var cfg *config.Config
			app := fxtest.New(t, bootstrap.App, fx.Populate(&cfg),
				// No lifecycle start: construct the graph, assert the shape,
				// then tear it down without opening sockets or goroutines.
				fx.NopLogger,
			)
			require.NoError(t, app.Err(), "mode %s must compose", tc.mode)
			require.NotNil(t, cfg)

			assert.Equal(t, tc.mode, cfg.Mode)
			assert.Equal(t, tc.wantWorkers, cfg.Mode.RunsWorkers(cfg.WorkersEnabled))
			assert.Equal(t, tc.wantHTTPRoute, cfg.Mode.ServesBusinessRoutes())
			assert.True(t, cfg.Mode.RunsHTTP(), "every mode exposes probes and /metrics")
		})
	}
}
