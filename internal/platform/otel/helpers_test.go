package otel

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	codes "go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// --- logs.go: the slog ↔ OTel bridge ----------------------------------------

func TestTracingAttrsHandlerAddsTraceAndSpanIDs(t *testing.T) {
	t.Parallel()
	inner := &recordingSlogHandler{}
	h := NewTracingAttrsHandler(inner)
	require.Equal(t, inner, h.inner)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ctx, span := tp.Tracer("test").Start(t.Context(), "op")

	var rec slog.Record
	require.NoError(t, h.Handle(ctx, rec))

	var keys []string
	inner.last.Attrs(func(a slog.Attr) bool { keys = append(keys, a.Key); return true })
	assert.Contains(t, keys, "trace_id")
	assert.Contains(t, keys, "span_id")
	span.End()
}

func TestTracingAttrsHandlerSkipsInvalidSpanContext(t *testing.T) {
	t.Parallel()
	inner := &recordingSlogHandler{}
	h := NewTracingAttrsHandler(inner)

	var rec slog.Record
	require.NoError(t, h.Handle(context.Background(), rec))

	var keys []string
	inner.last.Attrs(func(a slog.Attr) bool { keys = append(keys, a.Key); return true })
	assert.NotContains(t, keys, "trace_id")
	assert.NotContains(t, keys, "span_id")
}

func TestTracingAttrsHandlerDerivesChildren(t *testing.T) {
	t.Parallel()
	h := NewTracingAttrsHandler(&recordingSlogHandler{})
	assert.True(t, h.Enabled(context.Background(), slog.LevelInfo))
	assert.IsType(t, &TracingAttrsHandler{}, h.WithAttrs(nil))
	assert.IsType(t, &TracingAttrsHandler{}, h.WithGroup("g"))
}

// TestNewLoggerProviderDisabledReturnsNil covers the "stdout only" default:
// with a disabled provider (or no exporter) there is no OTLP log pipeline.
func TestNewLoggerProviderDisabledReturnsNil(t *testing.T) {
	t.Parallel()
	var p *Provider
	lp, err := p.NewLoggerProvider(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, lp)

	// Enabled provider but no exporter → still nil.
	enabled := &Provider{enabled: true}
	lp, err = enabled.NewLoggerProvider(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, lp)
}

// TestOTelHandlerIsNeverNil pins the composition contract.
func TestOTelHandlerIsNeverNil(t *testing.T) {
	t.Parallel()
	assert.NotNil(t, OTelHandler("wallet"))
}

// --- spans.go: semantic conventions -----------------------------------------

// spanAttr is a tiny SpanAttrLike used to drive the generic helpers.
type spanAttr struct {
	key string
	val any
}

func (a spanAttr) AttrKey() string { return a.key }
func (a spanAttr) AttrVal() any    { return a.val }

func TestKeyValuesStringifiesInOrder(t *testing.T) {
	t.Parallel()
	kvs := KeyValues(spanAttr{"a", 1}, spanAttr{"b", "x"})
	require.Len(t, kvs, 2)
	assert.Equal(t, "a", string(kvs[0].Key))
	assert.Equal(t, "1", kvs[0].Value.AsString())
	assert.Equal(t, "b", string(kvs[1].Key))
	assert.Equal(t, "x", kvs[1].Value.AsString())
}

func TestSpanStartOptionsAttachAtCreation(t *testing.T) {
	t.Parallel()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	_, span := tp.Tracer("test").Start(t.Context(), "op",
		SpanStartOptions(spanAttr{"wallet.queue", "wager"}))
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	require.Len(t, ended[0].Attributes(), 1)
	assert.Equal(t, "wallet.queue", string(ended[0].Attributes()[0].Key))
}

func TestTypedAttrBuildersKeepTypes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(3), IntAttr("n", 3).Value.AsInt64())
	assert.True(t, BoolAttr("b", true).Value.AsBool())
	assert.Equal(t, "s", StrAttr("s", "s").Value.AsString())
	assert.Equal(t, 1.5, FloatAttr("f", 1.5).Value.AsFloat64())
}

func TestRecordErrorIgnoresNil(t *testing.T) {
	t.Parallel()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	_, span := tp.Tracer("test").Start(t.Context(), "op")

	RecordError(span, nil)
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.NotEqual(t, codes.Error, ended[0].Status().Code)
	assert.Empty(t, ended[0].Events())
}

func TestRecordErrorRecordsCause(t *testing.T) {
	t.Parallel()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	_, span := tp.Tracer("test").Start(t.Context(), "op")

	RecordError(span, errors.New("boom"))
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, "boom", ended[0].Status().Description)
	assert.NotEmpty(t, ended[0].Events())
}

// --- helpers ----------------------------------------------------------------

// recordingSlogHandler captures the last record it handled.
type recordingSlogHandler struct{ last slog.Record }

func (h *recordingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.last = r
	return nil
}
func (h *recordingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingSlogHandler) WithGroup(string) slog.Handler      { return h }

// noopLogExporter is a valid sdklog.Exporter that drops everything. The point
// is not to assert on the records (the OTLP pipeline is exercised end to end
// against the Collector) but to drive NewLoggerProvider's ENABLED path, where
// the resource, batch processor and global provider are actually installed.
type noopLogExporter struct {
	shutdowns int
}

func (e *noopLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (e *noopLogExporter) Shutdown(context.Context) error {
	e.shutdowns++
	return nil
}
func (e *noopLogExporter) ForceFlush(context.Context) error { return nil }

// TestNewLoggerProviderEnabledBuildsPipeline covers the branch the disabled
// test cannot: an enabled provider plus a real exporter must build a logger
// provider (not nil) and install it as the global default, so OTelHandler
// downstream has somewhere to write.
func TestNewLoggerProviderEnabledBuildsPipeline(t *testing.T) {
	p := &Provider{enabled: true}
	exp := &noopLogExporter{}

	lp, err := p.NewLoggerProvider(context.Background(), exp)
	require.NoError(t, err)
	require.NotNil(t, lp, "an enabled provider with an exporter must build a pipeline")

	// The global provider is now the one we just built: shutting ours down
	// must reach the exporter (proving the wiring, not just the handle).
	require.NoError(t, lp.Shutdown(context.Background()))
	assert.Equal(t, 1, exp.shutdowns)
}
