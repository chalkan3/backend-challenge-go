//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// Direct (in-process) construction of the real services against the real
// database — used by scenarios that need precise control over retry policy
// timing, without touching the compose app instance.

func newTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	pool := mustDB(t)
	return postgres.NewStore(pool)
}

func newTestWagerService(t *testing.T, store *postgres.Store, maxAttempts int, ttl time.Duration) *service.WagerService {
	t.Helper()
	return service.NewWagerService(store, testLogger, &silentInstrumentation{},
		service.WithReferencePolicy(maxAttempts, ttl))
}

func externalInputFor(provider, extID, playerID, walletID string) service.ExternalInput {
	return service.ExternalInput{
		ProviderID:            provider,
		ExternalTransactionID: extID,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-test",
		GameID:                "fortune-chimp",
		ReceivedVia:           domain.ViaHTTP,
	}
}

// silentInstrumentation drops metrics on the floor for direct-service tests.
type silentInstrumentation struct{}

func (s *silentInstrumentation) WagerOutcome(string, string, string, string) {}
func (s *silentInstrumentation) Duplicate(string)                            {}
func (s *silentInstrumentation) ReferenceOutcome(string)                     {}
func (s *silentInstrumentation) ReconciliationDivergence()                   {}

// StartSpan returns a no-op span: these tests exercise the use cases against
// the real database, not the tracing pipeline.
func (s *silentInstrumentation) StartSpan(ctx context.Context, _ string, _ ...service.SpanAttr) (context.Context, service.Span) {
	return ctx, silentSpan{}
}

type silentSpan struct{}

func (silentSpan) SetAttributes(...service.SpanAttr) {}
func (silentSpan) SetOutcome(string, string)         {}
func (silentSpan) RecordError(error)                 {}
func (silentSpan) End()                              {}

var _ service.Instrumentation = (*silentInstrumentation)(nil)

// keep context imported for helpers used by other files in this package
var _ = context.Background
