//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/chalkan33/wallet/internal/bootstrap"
)

// TestFxCompositionAndLifecycle verifies the Fx graph resolves and that the
// application starts and stops cleanly, releasing worker and connection
// resources (challenge §13, "verificação da composição Fx").
func TestFxCompositionAndLifecycle(t *testing.T) {
	t.Setenv("WALLET_HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("WALLET_DB_DSN", dbDSN(t))
	t.Setenv("WALLET_SQS_ENDPOINT", sqsEndpoint(t))
	t.Setenv("WALLET_AUTH_ISSUER", keycloakURL(t)+"/realms/"+realm)
	t.Setenv("WALLET_AUTH_JWKS_URL", "")
	t.Setenv("WALLET_MIGRATE_ON_START", "false") // migrations covered by the compose app / CLI
	t.Setenv("WALLET_WORKERS_ENABLED", "true")
	t.Setenv("WALLET_OUTBOX_INTERVAL", "200ms")
	t.Setenv("WALLET_REFERENCE_INTERVAL", "200ms")

	// graph resolution without running anything
	probe := fx.New(bootstrap.App)
	if err := probe.Err(); err != nil {
		t.Fatalf("fx graph invalid: %v", err)
	}

	// full start/stop cycle: HTTP server binds, workers run, everything
	// stops within the grace period and resources are released.
	app := fx.New(bootstrap.App)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // let workers tick at least once

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("fx stop: %v", err)
	}
}
