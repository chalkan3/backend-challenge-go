// This file covers the telemetry composition root: the two providers and the
// lifecycle hook that flushes them on shutdown. These are the parts a normal
// run only touches when the observability stack is actually configured, which
// is exactly why they are worth pinning — a broken shutdown hook leaks spans
// and logs on every deploy, and nothing else in the suite would notice.
package bootstrap

import (
	"context"
	"log/slog"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/fx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
)

// capturingLifecycle is the smallest possible fx.Lifecycle: it records the
// hooks Append is given so a test can run the OnStop side by hand. Fx itself
// does nothing more than call these in order.
type capturingLifecycle struct {
	hooks []fx.Hook
}

func (l *capturingLifecycle) Append(h fx.Hook) { l.hooks = append(l.hooks, h) }

func (l *capturingLifecycle) stop(t *testing.T) {
	t.Helper()
	for _, h := range l.hooks {
		if h.OnStop == nil {
			continue
		}
		require.NoError(t, h.OnStop(context.Background()))
	}
}

// TestTelemetryDisabledIsNotFatal pins the availability-over-telemetry
// contract: a config with tracing off must still yield a usable (no-op)
// provider and a nil logger provider, both accepted by Fx.
func TestTelemetryDisabledIsNotFatal(t *testing.T) {
	cfg := &config.Config{TelemetryEnabled: false}

	p, err := newTelemetryProvider(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.False(t, p.Enabled())

	lp, err := newLoggerProvider(cfg, p)
	require.NoError(t, err)
	assert.Nil(t, lp, "log export off ⇒ nil provider (a valid Fx no-op)")
}

// TestLoggerProviderRequiresBothSwitches pins the AND between tracing and log
// export: turning logs on while tracing is off must NOT build an OTLP log
// exporter (there would be no provider to attach the resource to).
func TestLoggerProviderRequiresBothSwitches(t *testing.T) {
	p, err := otel.New(context.Background(), otel.Config{Enabled: false})
	require.NoError(t, err)

	// Logs on, tracing off.
	lp, err := newLoggerProvider(&config.Config{
		TelemetryEnabled:     false,
		TelemetryLogsEnabled: true,
	}, p)
	require.NoError(t, err)
	assert.Nil(t, lp)
}

// TestRegisterTelemetryFlushesOnStop runs the shutdown hook that Fx schedules.
// With tracing disabled the providers shut down as no-ops, but the hook body —
// including the warning branches — is still executed, so a nil-provider panic
// or a missing flush would fail here.
func TestRegisterTelemetryFlushesOnStop(t *testing.T) {
	p, err := otel.New(context.Background(), otel.Config{Enabled: false})
	require.NoError(t, err)

	lc := &capturingLifecycle{}
	registerTelemetry(lc, p, nil, slog.Default())

	require.Len(t, lc.hooks, 1, "registerTelemetry must append exactly one stop hook")
	lc.stop(t)
}

// TestNewLoggerProviderEnabledBuildsExporter covers the branch the disabled
// test cannot: both switches on. The exporter is built against an unreachable
// endpoint on purpose — otlploggrpc.New does not dial eagerly, so construction
// succeeds and the OTLP pipeline is really assembled.
func TestNewLoggerProviderEnabledBuildsExporter(t *testing.T) {
	p, err := otel.New(context.Background(), otel.Config{Enabled: true, Endpoint: "127.0.0.1:1", Insecure: true})
	require.NoError(t, err)

	lp, err := newLoggerProvider(&config.Config{
		TelemetryEnabled:     true,
		TelemetryLogsEnabled: true,
		TelemetryEndpoint:    "127.0.0.1:1", // unreachable, but New() still succeeds
	}, p)
	require.NoError(t, err)
	require.NotNil(t, lp, "both switches on must build a logger provider")
	require.NoError(t, lp.Shutdown(context.Background()))
}

// TestRegisterTelemetryWithLogProvider exercises the shutdown hook when a log
// provider IS present and tracing IS enabled: both Shutdown calls run, and the
// "telemetry flushed" line is emitted. This is the branch a real deployment
// takes, and a nil-deref or an early return here would leak on every shutdown.
func TestRegisterTelemetryWithLogProvider(t *testing.T) {
	exp := &noopBootstrapLogExporter{}
	tp, err := otel.New(context.Background(), otel.Config{Enabled: true, Endpoint: "127.0.0.1:1", Insecure: true})
	require.NoError(t, err)

	lp, err := tp.NewLoggerProvider(context.Background(), exp)
	require.NoError(t, err)
	require.NotNil(t, lp)

	lc := &capturingLifecycle{}
	registerTelemetry(lc, tp, lp, slog.Default())

	require.Len(t, lc.hooks, 1)
	// OnStop must reach the log provider's exporter, proving the wiring.
	require.NoError(t, lc.hooks[0].OnStop(context.Background()))
	assert.Equal(t, 1, exp.shutdowns)
}

// noopBootstrapLogExporter is a valid sdklog.Exporter that drops records and
// counts shutdowns, so a test can prove the OnStop hook reaches it.
type noopBootstrapLogExporter struct{ shutdowns int }

func (e *noopBootstrapLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (e *noopBootstrapLogExporter) Shutdown(context.Context) error {
	e.shutdowns++
	return nil
}
func (e *noopBootstrapLogExporter) ForceFlush(context.Context) error { return nil }
