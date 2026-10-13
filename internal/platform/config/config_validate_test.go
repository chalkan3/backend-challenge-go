package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validate is the guard that refuses to boot a misconfigured process
// (challenge section 4, lifecycle). Each branch protects one class of silent
// failure, so every one is asserted to fire on its own input.

func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	require.NoError(t, err, "the all-defaults environment must be valid")
	require.NoError(t, cfg.validate())
	return cfg
}

func TestValidateRejectsEachMisconfigurationIndependently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing http addr", func(c *Config) { c.HTTPAddr = "" }, "WALLET_HTTP_ADDR"},
		{"missing db dsn", func(c *Config) { c.DBDSN = "" }, "WALLET_DB_DSN"},
		{"zero max conns", func(c *Config) { c.DBMaxConns = 0 }, "WALLET_DB_MAX_CONNS"},
		{"missing region", func(c *Config) { c.SQSRegion = "" }, "WALLET_SQS_REGION"},
		{"missing queue name", func(c *Config) { c.SQSDLQQueue = "" }, "SQS queue names are required"},
		{"non-fifo queue", func(c *Config) { c.SQSRequestQueue = "wager-transactions" }, "SQS queues must be .fifo"},
		{"negative wait seconds", func(c *Config) { c.SQSWaitSeconds = -1 }, "WALLET_SQS_WAIT_SECONDS"},
		{"too many messages", func(c *Config) { c.SQSMaxMessages = 11 }, "WALLET_SQS_MAX_MESSAGES"},
		{"missing issuer", func(c *Config) { c.AuthIssuer = "" }, "WALLET_AUTH_ISSUER"},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }, "WALLET_LOG_LEVEL"},
		{"zero concurrency", func(c *Config) { c.ConsumerConcurrency = 0 }, "WALLET_CONSUMER_CONCURRENCY"},
		{"zero reference attempts", func(c *Config) { c.ReferenceMaxAttempts = 0 }, "reference retry policy"},
		{"non-positive reference ttl", func(c *Config) { c.ReferenceTTL = 0 }, "reference retry policy"},
		{"sample ratio above one", func(c *Config) { c.TelemetrySampleRatio = 1.5 }, "WALLET_OTEL_SAMPLE_RATIO"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig(t)
			tc.mutate(cfg)

			err := cfg.validate()
			require.Error(t, err, "the misconfiguration must be rejected")
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

// TestValidateAccumulatesEveryProblem pins the "report all, not just the
// first" behaviour: an operator fixing config one failure per boot is a worse
// experience than seeing the whole list at once.
func TestValidateAccumulatesEveryProblem(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.HTTPAddr = ""
	cfg.DBDSN = ""
	cfg.LogLevel = "nope"

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WALLET_HTTP_ADDR")
	assert.Contains(t, err.Error(), "WALLET_DB_DSN")
	assert.Contains(t, err.Error(), "WALLET_LOG_LEVEL")
}

// TestTelemetryValidationPropagates checks the delegation: validate must
// surface the telemetry package's own error (tracing on without endpoint).
func TestTelemetryValidationPropagates(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	cfg.TelemetryEnabled = true
	cfg.TelemetryEndpoint = ""

	err := cfg.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}

// TestEnvHelpersFallBackOnGarbage covers the parsers: an unparseable value
// must fall back to the default rather than yielding a zero value that would
// silently disable a feature.
func TestEnvHelpersFallBackOnGarbage(t *testing.T) {
	t.Setenv("WALLET_TEST_INT", "not-a-number")
	t.Setenv("WALLET_TEST_BOOL", "maybe")
	t.Setenv("WALLET_TEST_FLOAT", "1.2.3")
	t.Setenv("WALLET_TEST_DURATION", "soon")

	assert.Equal(t, 7, envInt("WALLET_TEST_INT", 7))
	assert.True(t, envBool("WALLET_TEST_BOOL", true))
	assert.Equal(t, 2.5, envFloat("WALLET_TEST_FLOAT", 2.5))
	assert.Equal(t, 100, int(envInt32("WALLET_TEST_INT32_MISSING", 100)))
}

func TestEnvHelpersParseValidValues(t *testing.T) {
	t.Setenv("WALLET_TEST_INT_OK", "42")
	t.Setenv("WALLET_TEST_BOOL_OK", "true")
	t.Setenv("WALLET_TEST_FLOAT_OK", "0.25")

	assert.Equal(t, 42, envInt("WALLET_TEST_INT_OK", 0))
	assert.True(t, envBool("WALLET_TEST_BOOL_OK", false))
	assert.Equal(t, 0.25, envFloat("WALLET_TEST_FLOAT_OK", 1.0))
}

func TestEnvEmptyStringFallsBackToDefault(t *testing.T) {
	t.Setenv("WALLET_TEST_EMPTY", "")
	assert.Equal(t, "fallback", env("WALLET_TEST_EMPTY", "fallback"))
	assert.Equal(t, 5, envInt("WALLET_TEST_MISSING_INT", 5))
}

func TestHostnameOrDefault(t *testing.T) {
	t.Parallel()
	got := hostnameOr("unknown")
	assert.NotEmpty(t, got)
}
