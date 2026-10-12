package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// The use cases and the workers own their span vocabulary (so they never
// import a tracing SDK), while the otel package owns the canonical
// conventions the dashboards query. Nothing at compile time keeps the two in
// sync, so these parity tests are the guard: a renamed span or attribute in
// one place must be renamed in the other, or the dashboards silently go
// blank. They run here, in the composition root, which imports every layer.
func TestSpanNameParity(t *testing.T) {
	t.Parallel()

	assert.Equal(t, otel.SpanWagerExecute, service.SpanWagerExecute)
	assert.Equal(t, otel.SpanWagerPrepare, service.SpanWagerPrepare)
	assert.Equal(t, otel.SpanWagerResolve, service.SpanWagerResolve)
	assert.Equal(t, otel.SpanWalletOpen, service.SpanWalletOpen)
	assert.Equal(t, otel.SpanWalletMovement, service.SpanWalletMovement)

	assert.Equal(t, otel.SpanConsumerHandle, messaging.SpanConsumerHandle)
	assert.Equal(t, otel.SpanOutboxPublish, messaging.SpanOutboxPublish)
	assert.Equal(t, otel.SpanReferenceTick, messaging.SpanReferenceTick)
}

func TestSpanAttributeParity(t *testing.T) {
	t.Parallel()

	// service attributes
	assert.Equal(t, string(otel.AttrKind), service.AttrKind)
	assert.Equal(t, string(otel.AttrStatus), service.AttrStatus)
	assert.Equal(t, string(otel.AttrFailureCode), service.AttrFailureCode)
	assert.Equal(t, string(otel.AttrChannel), service.AttrChannel)
	assert.Equal(t, string(otel.AttrDuplicate), service.AttrDuplicate)
	assert.Equal(t, string(otel.AttrDuplicateSrc), service.AttrDuplicateSrc)
	assert.Equal(t, string(otel.AttrProviderID), service.AttrProviderID)
	assert.Equal(t, string(otel.AttrWalletID), service.AttrWalletID)
	assert.Equal(t, string(otel.AttrPlayerID), service.AttrPlayerID)
	assert.Equal(t, string(otel.AttrBalanceMinor), service.AttrBalanceMinor)

	// messaging attributes
	assert.Equal(t, string(otel.AttrMessageID), messaging.AttrMessageID)
	assert.Equal(t, string(otel.AttrDuplicate), messaging.AttrDuplicate)
	assert.Equal(t, string(otel.AttrDuplicateSrc), messaging.AttrDuplicateSrc)
}

// TestAdaptersSatisfyPorts pins the wiring contract: a refactor that breaks
// a port from the adapters fails here, at compile time, instead of at
// runtime in production.
func TestAdaptersSatisfyPorts(t *testing.T) {
	t.Parallel()

	var _ service.Instrumentation = (*instrumentation)(nil)
	var _ messaging.Instrumentation = (*messagingAdapter)(nil)
	var _ messaging.ConsumerMetrics = (*messagingAdapter)(nil)
}
