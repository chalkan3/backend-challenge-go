package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mode is the whole reason this service is one binary with three entry
// points instead of three binaries, so its role matrix is pinned here: a
// regression that makes "api" start workers (or "workers" serve business
// traffic) would silently break the topology argument in the README.
func TestModeRoleMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode           Mode
		httpBusiness   bool
		workersWithOn  bool
		workersWithOff bool
	}{
		{mode: ModeAll, httpBusiness: true, workersWithOn: true, workersWithOff: false},
		{mode: ModeAPI, httpBusiness: true, workersWithOn: false, workersWithOff: false},
		{mode: ModeWorkers, httpBusiness: false, workersWithOn: true, workersWithOff: true},
	}
	for _, tc := range tests {
		t.Run(tc.mode.String(), func(t *testing.T) {
			t.Parallel()
			assert.True(t, tc.mode.Valid())
			// Every mode keeps /health/* and /metrics: probes and scraping
			// must not depend on the role.
			assert.True(t, tc.mode.RunsHTTP())
			assert.Equal(t, tc.httpBusiness, tc.mode.ServesBusinessRoutes())
			assert.Equal(t, tc.workersWithOn, tc.mode.RunsWorkers(true),
				"workers mode runs workers even when the kill switch is off")
			assert.Equal(t, tc.workersWithOff, tc.mode.RunsWorkers(false))
		})
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    Mode
		wantErr bool
	}{
		{raw: "", want: ModeAll, wantErr: false},
		{raw: "all", want: ModeAll},
		{raw: "api", want: ModeAPI},
		{raw: "workers", want: ModeWorkers},
		{raw: "API", wantErr: true},
		{raw: "worker", wantErr: true},
		{raw: "nope", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, err := parseMode(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestLoadModeFromEnv guards the boundary: the mode reaches the app only
// through WALLET_MODE, and an unknown value stops the process at config load
// instead of degrading into a default nobody intended.
func TestLoadModeFromEnv(t *testing.T) {
	t.Setenv("WALLET_MODE", "workers")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ModeWorkers, cfg.Mode)
	assert.False(t, cfg.Mode.ServesBusinessRoutes())
	assert.True(t, cfg.Mode.RunsWorkers(cfg.WorkersEnabled))

	t.Setenv("WALLET_MODE", "banana")
	_, err = Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WALLET_MODE")
}
