//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestWalletOpenIdempotencyAgainstRealStack pins the durable idempotency of
// POST /wallets against the REAL stack (PostgreSQL + Keycloak), where the
// guarantee has to hold: the idempotency key and the wallet (plus its OPENING
// ledger row) commit in the same transaction, so a retried opening replays the
// original result instead of creating a second wallet or double-spending the
// opening balance.
func TestWalletOpenIdempotencyAgainstRealStack(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	key := "open-" + uuid.NewString()[:12]

	body := map[string]any{
		"playerId":       player,
		"initialBalance": map[string]string{"amount": "100.00", "currency": "BRL"},
	}

	// First attempt creates the wallet.
	st, first := apiWithHeader(t, apiURL(t), "POST", "/wallets", internal, body, "Idempotency-Key", key)
	if st != http.StatusCreated {
		t.Fatalf("first open: %d %v", st, first)
	}
	if first["idempotentReplay"] == true {
		t.Fatalf("first open reported a replay: %v", first)
	}
	walletID, _ := first["id"].(string)
	if walletID == "" {
		t.Fatalf("first open returned no id: %v", first)
	}

	// Replay of the same key + payload: same wallet, no new ledger row.
	st, second := apiWithHeader(t, apiURL(t), "POST", "/wallets", internal, body, "Idempotency-Key", key)
	if st != http.StatusCreated {
		t.Fatalf("replay: %d %v", st, second)
	}
	if second["id"] != walletID {
		t.Fatalf("replay returned a different wallet: %v vs %s", second["id"], walletID)
	}
	if second["idempotentReplay"] != true {
		t.Fatalf("replay not flagged as idempotent: %v", second)
	}

	// The ledger must carry exactly ONE OPENING entry: no double credit.
	pool := mustDB(t)
	var entries, credits int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, walletID).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || credits != 1 {
		t.Fatalf("ledger entries=%d credits=%d, want exactly 1 and 1 (opening never duplicated)", entries, credits)
	}

	// Reusing the key with a DIFFERENT payload is a conflict, not a replay.
	changed := map[string]any{
		"playerId":       player,
		"initialBalance": map[string]string{"amount": "250.00", "currency": "BRL"},
	}
	st, conflict := apiWithHeader(t, apiURL(t), "POST", "/wallets", internal, changed, "Idempotency-Key", key)
	if st != http.StatusConflict {
		t.Fatalf("key reuse with new payload: %d %v, want 409", st, conflict)
	}
	if code := errCode(conflict); code != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Fatalf("conflict code = %v, want IDEMPOTENCY_KEY_CONFLICT", code)
	}

	// Balance is untouched by any of the above.
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("balance = %s, want 100.00", bal)
	}
}
