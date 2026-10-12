//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestAuthenticationAndAuthorization covers the eliminatory requirements:
// real IdP integration, rejection of missing/invalid credentials, provider
// isolation and restriction of internal operations.
func TestAuthenticationAndAuthorization(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	providerB := token(t, providerBClient, providerBSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")

	// 1. no credentials → 401
	st, _ := api(t, "GET", "/wallets/"+walletID, "", nil)
	if st != 401 {
		t.Fatalf("no token: %d", st)
	}
	st, _ = api(t, "POST", "/wagering/transactions", "", betBody("provider-a", "x", player, walletID, "1.00"))
	if st != 401 {
		t.Fatalf("no token submit: %d", st)
	}

	// 2. invalid credentials → 401
	st, _ = api(t, "GET", "/wallets/"+walletID, "garbage.token.here", nil)
	if st != 401 {
		t.Fatalf("garbage token: %d", st)
	}

	// 3. token from a token-less audience... tampered signature → 401
	tampered := providerA[:len(providerA)-4] + "AAAA"
	st, _ = api(t, "GET", "/wallets/"+walletID, tampered, nil)
	if st != 401 {
		t.Fatalf("tampered token: %d", st)
	}

	// 4. providers cannot operate wallets (internal-only) → 403
	st, _ = api(t, "POST", "/wallets", providerA, map[string]any{
		"playerId":       newPlayerID(t),
		"initialBalance": map[string]string{"amount": "10.00", "currency": "BRL"},
	})
	if st != 403 {
		t.Fatalf("provider opening wallet: %d", st)
	}
	st, _ = api(t, "GET", "/wallets/"+walletID, providerA, nil)
	if st != 403 {
		t.Fatalf("provider reading wallet: %d", st)
	}

	// 5. provider isolation on submissions → 403 and NO financial effect
	extID := "isolation-" + uuid.NewString()[:8]
	st, body := submitTransaction(t, providerA, betBody("provider-b", extID, player, walletID, "10.00"), "provider-b:"+extID)
	if st != 403 {
		t.Fatalf("provider-a submitting as provider-b: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("unauthorized submit moved money: %s", bal)
	}

	// 6. provider isolation on reads: provider-b cannot read provider-a tx
	betID := "iso-bet-" + uuid.NewString()[:8]
	st, _ = submitTransaction(t, providerA, betBody("provider-a", betID, player, walletID, "5.00"), "provider-a:"+betID)
	if st != 200 {
		t.Fatalf("setup bet: %d", st)
	}
	// 404 (not 403): asking for another provider's external id under your
	// own providerId must not leak its existence.
	st, _ = api(t, "GET", "/providers/provider-b/wagering/transactions/"+betID, providerB, nil)
	if st != 404 {
		t.Fatalf("cross-provider read: %d, want 404 (no existence leak)", st)
	}
	// provider-a reads its own
	st, body = api(t, "GET", "/providers/provider-a/wagering/transactions/"+betID, providerA, nil)
	if st != 200 || body["externalTransactionId"] != betID {
		t.Fatalf("own read: %d %v", st, body)
	}
	// generic endpoint: provider-b reads provider-a's transaction by id → 403
	txID, _ := body["transactionId"].(string)
	st, _ = api(t, "GET", "/wagering/transactions/"+txID, providerB, nil)
	if st != 403 {
		t.Fatalf("provider-b reading provider-a tx by id: %d", st)
	}
	// internal reads anything
	st, _ = api(t, "GET", "/wagering/transactions/"+txID, internal, nil)
	if st != 200 {
		t.Fatalf("internal read: %d", st)
	}

	// 7. unknown client credentials are rejected by the IdP itself
	fakeBody := fmt.Sprintf("grant_type=client_credentials&client_id=ghost&client_secret=%s", uuid.NewString())
	resp, err := clientDo(t, "POST", keycloakURL(t)+"/realms/"+realm+"/protocol/openid-connect/token", fakeBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 && resp.StatusCode != 400 {
		t.Fatalf("unknown client: %d", resp.StatusCode)
	}
}
