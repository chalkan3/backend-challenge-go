//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/bootstrap"
)

// freePort reserves an ephemeral local port and releases it for the app to
// bind (TOCTOU window is acceptable inside the test environment).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

// startAppInstance builds and starts a full Fx application instance against
// the shared stack. env is set before construction because config.Load runs
// inside the fx graph; each fx.New is an independent graph.
func startAppInstance(t *testing.T, httpAddr string, workers bool) *fx.App {
	t.Helper()
	t.Setenv("WALLET_HTTP_ADDR", httpAddr)
	t.Setenv("WALLET_DB_DSN", dbDSN(t))
	t.Setenv("WALLET_SQS_ENDPOINT", sqsEndpoint(t))
	t.Setenv("WALLET_AUTH_ISSUER", keycloakURL(t)+"/realms/"+realm)
	t.Setenv("WALLET_AUTH_JWKS_URL", "")
	t.Setenv("WALLET_MIGRATE_ON_START", "false")
	t.Setenv("WALLET_SQS_WAIT_SECONDS", "1") // short long-poll: minimal zombie-receiver window on stop
	if workers {
		t.Setenv("WALLET_WORKERS_ENABLED", "true")
	} else {
		t.Setenv("WALLET_WORKERS_ENABLED", "false")
	}
	t.Setenv("WALLET_OUTBOX_INTERVAL", "200ms")
	t.Setenv("WALLET_REFERENCE_INTERVAL", "200ms")

	a := fx.New(bootstrap.App)
	if err := a.Err(); err != nil {
		t.Fatalf("fx graph invalid: %v", err)
	}
	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Start(startCtx); err != nil {
		t.Fatalf("fx start (%s): %v", httpAddr, err)
	}
	return a
}

func stopAppInstance(t *testing.T, a *fx.App) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Stop(stopCtx); err != nil {
		t.Fatalf("fx stop: %v", err)
	}
}

// TestRestartAnotherInstanceResumesPending is the §13.8 mandatory scenario:
// instance A accepts a REFUND whose reference has not arrived (durable
// PENDING_REFERENCE) and is then shut down. A restarted instance B preserves
// idempotency, resumes the pending operation once the reference arrives, and
// the financial result stays consistent.
func TestRestartAnotherInstanceResumesPending(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	base := uuid.NewString()[:8]

	// ---- instance A: workers disabled, accepts and persists the pending ----
	addrA := freePort(t)
	appA := startAppInstance(t, addrA, false)

	refID := "restart-ref-" + base
	refundBody := map[string]any{
		"providerId": "provider-a", "externalTransactionId": refID,
		"playerId": player, "walletId": walletID, "roundId": "round-restart", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": map[string]string{"amount": "25.00", "currency": "BRL"},
		"referenceExternalTransactionId": "restart-bet-" + base,
	}
	key := "provider-a:" + refID
	st, body := apiWithHeader(t, "http://"+addrA, "POST", "/wagering/transactions", providerA, refundBody, "Idempotency-Key", key)
	if st != 202 || body["status"] != "PENDING_REFERENCE" {
		t.Fatalf("instance A refund: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("pending must not move money: %s", bal)
	}

	// ---- instance A dies with the pending committed ----------------------
	stopAppInstance(t, appA)

	// ---- restart: instance B, workers enabled ----------------------------
	addrB := freePort(t)
	appB := startAppInstance(t, addrB, true)
	defer stopAppInstance(t, appB)

	// the referenced BET arrives at the restarted instance
	b := betBody("provider-a", "restart-bet-"+base, player, walletID, "25.00")
	b["roundId"] = "round-restart"
	if st, body := submitTransaction(t, providerA, b, "provider-a:restart-bet-"+base); st != 200 || body["status"] != "PROCESSED" {
		t.Fatalf("instance B bet: %d %v", st, body)
	}

	// instance B's reference worker resumes A's pending refund
	pollUntil(t, 20*time.Second, func() bool {
		_, b := api(t, "GET", "/providers/provider-a/wagering/transactions/"+refID, providerA, nil)
		return b["status"] == "PROCESSED"
	}, "restarted instance never resumed the pending refund")

	// financial consistency preserved across the restart:
	// opening credit 100.00 - bet 25.00 + refund 25.00 → 3 entries, 100.00.
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("balance after restart resumption: %s, want 100.00", bal)
	}
	pool := mustDB(t)
	var entries int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 3 { // opening credit + bet debit + refund credit
		t.Fatalf("ledger entries: %d, want 3", entries)
	}
	var debits int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, walletID).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if debits != 1 { // exactly one debit: the referenced bet
		t.Fatalf("debit entries: %d, want 1", debits)
	}

	// idempotency preserved: replaying A's refund (same key) returns the
	// persisted result instead of applying anything new.
	st, body = submitTransaction(t, providerA, refundBody, key)
	if st != 200 || body["idempotentReplay"] != true {
		t.Fatalf("replay after restart: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("balance altered by replay: %s", bal)
	}
}
