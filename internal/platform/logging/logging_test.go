// Package logging tests pin the two behaviours that make the logger useful in
// production: the level mapping from configuration and the correlation
// attributes carried by the context. Both are contracts the dashboards and
// the log-search queries rely on.
package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

func TestLevelMapsConfigurationStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo}, // case-insensitive
		{"Warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo}, // unknown → info
		{"nonsense", slog.LevelInfo},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, level(tc.in))
		})
	}
}

func TestNewWithWriterHonoursLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := &config.Config{LogLevel: "warn"}
	log := NewWithWriter(cfg, nil, &buf)

	log.Info("dropped") // below the threshold
	log.Warn("kept")    // at the threshold

	out := buf.String()
	assert.NotContains(t, out, "dropped")
	assert.Contains(t, out, "kept")
}

func TestNewWithWriterEmitsJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewWithWriter(&config.Config{LogLevel: "debug"}, nil, &buf)
	log.Info("hello", slog.String("k", "v"))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec), "the stdout contract is JSON")
	assert.Equal(t, "hello", rec["msg"])
	assert.Equal(t, "v", rec["k"])
	assert.Equal(t, "INFO", rec["level"])
}

func TestCorrelationRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := WithCorrelation(context.Background(), "corr-1")
	ctx = WithMessage(ctx, "msg-1")

	assert.Equal(t, "corr-1", CorrelationFrom(ctx))
	assert.Equal(t, "msg-1", MessageFrom(ctx))

	attrs := AttrsFrom(ctx)
	// Two slog attributes, in a stable order.
	require.Len(t, attrs, 2)
	assert.Equal(t, slog.String("correlationId", "corr-1"), attrs[0])
	assert.Equal(t, slog.String("messageId", "msg-1"), attrs[1])
}

func TestAttrsFromEmptyContextIsEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, AttrsFrom(context.Background()))
}

func TestAttrsFromIncludesOnlyPresentKeys(t *testing.T) {
	t.Parallel()
	only := AttrsFrom(WithMessage(context.Background(), "m"))
	require.Len(t, only, 1)
	assert.Equal(t, slog.String("messageId", "m"), only[0])
}

// TestCorrelationAttrsAreLoggedEndToEnd proves the pieces fit: a logger built
// by this package, called with the correlation context, emits the ids on the
// JSON line — which is what makes a log line findable by correlation id.
func TestCorrelationAttrsAreLoggedEndToEnd(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewWithWriter(&config.Config{LogLevel: "info"}, nil, &buf)

	ctx := WithCorrelation(context.Background(), "corr-e2e")
	ctx = WithMessage(ctx, "msg-e2e")
	log.InfoContext(ctx, "wager processed", AttrsFrom(ctx)...)

	out := buf.String()
	assert.True(t, strings.Contains(out, `"correlationId":"corr-e2e"`), out)
	assert.True(t, strings.Contains(out, `"messageId":"msg-e2e"`), out)
}

func TestFanoutHandlerCombinesEnabled(t *testing.T) {
	t.Parallel()
	// a is enabled at warn+, b is enabled everywhere: the fan-out is enabled
	// if either side is.
	a := &captureHandler{enabled: false}
	b := &captureHandler{enabled: true}
	f := &fanoutHandler{a: a, b: b}
	assert.True(t, f.Enabled(context.Background(), slog.LevelDebug))

	f2 := &fanoutHandler{a: &captureHandler{enabled: false}, b: &captureHandler{enabled: false}}
	assert.False(t, f2.Enabled(context.Background(), slog.LevelError))
}

func TestFanoutHandlerForwardsAndSwallowsSecondaryErrors(t *testing.T) {
	t.Parallel()
	primary := &captureHandler{enabled: true}
	secondary := &captureHandler{enabled: true, handleErr: assert.AnError}
	f := &fanoutHandler{a: primary, b: secondary}

	err := f.Handle(context.Background(), slog.Record{})
	assert.NoError(t, err, "a failing secondary sink must not fail the log call")
	assert.Equal(t, 1, primary.handled)
	assert.Equal(t, 1, secondary.handled)
}

func TestFanoutHandlerSkipsDisabledSecondary(t *testing.T) {
	t.Parallel()
	primary := &captureHandler{enabled: true}
	secondary := &captureHandler{enabled: false}
	f := &fanoutHandler{a: primary, b: secondary}

	require.NoError(t, f.Handle(context.Background(), slog.Record{}))
	assert.Equal(t, 1, primary.handled)
	assert.Zero(t, secondary.handled, "a disabled sink must not receive records")
}

func TestFanoutHandlerDerivesChildren(t *testing.T) {
	t.Parallel()
	f := &fanoutHandler{a: &captureHandler{}, b: &captureHandler{}}
	assert.IsType(t, &fanoutHandler{}, f.WithAttrs(nil))
	assert.IsType(t, &fanoutHandler{}, f.WithGroup("g"))
}

// captureHandler is a minimal slog.Handler that records calls.
type captureHandler struct {
	enabled   bool
	handleErr error
	handled   int
	groups    int
	attrs     int
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return h.enabled }

func (h *captureHandler) Handle(context.Context, slog.Record) error {
	h.handled++
	return h.handleErr
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { h.attrs++; return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { h.groups++; return h }
