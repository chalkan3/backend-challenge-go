//go:build integration

// Journey tests. Unlike flow_test.go — which asserts one invariant per endpoint
// — these walk a complete operator story end to end, in order, the way a human
// validating the challenge would narrate it. They are deliberately readable:
// each numbered step is one observable claim about the product.
//
// They reuse the same helpers (openWallet, submitTransaction, walletBalance,
// pollUntil) and the same real stack, so they cost an extra HTTP round trip,
// not an extra harness.
package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
)

// TestJourneyFullPlayerLifecycle walks one player from wallet opening through a
// bet, a win, a rollback of the bet, and a final reconciliation — checking the
// balance after every single step, plus the derived ledger and the event that
// must reach SQS.
//
// Read top to bottom, it is the product's core story:
//
//	open 100.00 → BET 30.00 → WIN 12.50 → ROLLBACK(BET) 30.00 → reconcile
//
// Expected balance arithmetic (minor units, BRL):
//
//	opening       +10000
//	BET   -3000  →  7000
//	WIN   +1250  →  8250
//	ROLLBACK +3000 → 11250  → "112.50"
func TestJourneyFullPlayerLifecycle(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	provider := token(t, providerAClient, providerASecret)

	// ── Step 1: the wallet does not exist yet. ──────────────────────────────
	player := newPlayerID(t)

	// ── Step 2: internal identity opens it with 100.00. ─────────────────────
	walletID := openWallet(t, internal, player, "100.00")
	if bal, version := walletBalance(t, internal, walletID); bal != "100.00" || version != 1 {
		t.Fatalf("step 2: balance=%s version=%d, want 100.00/1", bal, version)
	}

	// ── Step 3: the provider places a 30.00 BET. ────────────────────────────
	betExt := "journey-bet-" + uuid.NewString()[:8]
	roundID := "journey-round-" + uuid.NewString()[:8]
	// A reversal must reference an operation in the SAME round (domain rule:
	// checkReferenceAgreement), so every step below reuses roundID.
	betPayload := betBody(providerAClient, betExt, player, walletID, "30.00")
	betPayload["roundId"] = roundID
	status, body := submitTransaction(t, provider, betPayload, "pa:"+betExt)
	if status != http.StatusOK || body["status"] != "PROCESSED" {
		t.Fatalf("step 3 bet: %d %v", status, body)
	}
	betTxID, _ := body["transactionId"].(string)
	if betTxID == "" {
		t.Fatalf("step 3: no transactionId in %v", body)
	}
	if bal, ver := walletBalance(t, internal, walletID); bal != "70.00" || ver != 2 {
		t.Fatalf("step 3: balance=%s version=%d, want 70.00/2", bal, ver)
	}

	// ── Step 4: a 12.50 WIN credits the player. ─────────────────────────────
	winBody := map[string]any{
		"providerId":            providerAClient,
		"externalTransactionId": "journey-win-" + uuid.NewString()[:8],
		"playerId":              player,
		"walletId":              walletID,
		"roundId":               roundID, // same round as the bet
		"gameId":                "fortune-chimp",
		"kind":                  "WIN",
		"money":                 map[string]string{"amount": "12.50", "currency": "BRL"},
	}
	status, body = submitTransaction(t, provider, winBody, "pa:e2e-win-"+roundID)
	if status != http.StatusOK || body["status"] != "PROCESSED" {
		t.Fatalf("step 4 win: %d %v", status, body)
	}
	if bal, ver := walletBalance(t, internal, walletID); bal != "82.50" || ver != 3 {
		t.Fatalf("step 4: balance=%s version=%d, want 82.50/3", bal, ver)
	}

	// ── Step 5: the round is voided — a ROLLBACK of the original BET. ───────
	revBody := map[string]any{
		"providerId":                     providerAClient,
		"externalTransactionId":          "journey-rollback-" + uuid.NewString()[:8],
		"playerId":                       player,
		"walletId":                       walletID,
		"roundId":                        roundID,
		"gameId":                         "fortune-chimp",
		"kind":                           "ROLLBACK",
		"money":                          map[string]string{"amount": "30.00", "currency": "BRL"},
		"referenceExternalTransactionId": betExt, // which original operation is reversed
	}
	status, body = submitTransaction(t, provider, revBody, "pa:e2e-rev-"+roundID)
	if status != http.StatusOK || body["status"] != "PROCESSED" {
		t.Fatalf("step 5 reversal: %d %v", status, body)
	}
	if bal, ver := walletBalance(t, internal, walletID); bal != "112.50" || ver != 4 {
		t.Fatalf("step 5: balance=%s version=%d, want 112.50/4", bal, ver)
	}

	// ── Step 6: every operation is on the append-only ledger, in order. ─────
	status, ledger := api(t, "GET", "/wallets/"+walletID+"/ledger?limit=50", internal, nil)
	if status != http.StatusOK {
		t.Fatalf("step 6 ledger: %d %v", status, ledger)
	}
	items, _ := ledger["items"].([]any)
	if len(items) != 4 { // opening + bet + win + reversal
		t.Fatalf("step 6: ledger has %d entries, want 4: %v", len(items), items)
	}

	// ── Step 7: the provider can read back its own transaction by external id.
	status, readBack := api(t, "GET",
		"/providers/"+providerAClient+"/wagering/transactions/"+betExt, provider, nil)
	if status != http.StatusOK || readBack["transactionId"] != betTxID {
		t.Fatalf("step 7: %d %v (want transactionId=%s)", status, readBack, betTxID)
	}

	// ── Step 8: reconciliation proves the stored balance equals the ledger. ─
	status, rec := api(t, "POST", "/wallets/"+walletID+"/reconciliation", internal, nil)
	if status != http.StatusOK || rec["consistent"] != true {
		t.Fatalf("step 8: %d %v", status, rec)
	}
	if diff, _ := rec["difference"].(map[string]any)["amount"].(string); diff != "0.00" {
		t.Fatalf("step 8: difference=%v", rec["difference"])
	}

	// ── Step 9: the operations were announced on the integration queue. ─────
	// The outbox relay publishes asynchronously, so poll rather than assume.
	// The wire shape is the challenge envelope: {messageId,type,occurredAt,data}.
	client := mustSQS(t)
	queue := mustQueueURL(t, client, "integration-events.fifo")
	pollUntil(t, 20*time.Second, func() bool {
		out, err := client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(queue),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil {
			return false
		}
		for _, m := range out.Messages {
			if m.Body == nil {
				continue
			}
			var env struct {
				EventType string `json:"eventType"`
				Data      struct {
					ExternalTransactionID string `json:"externalTransactionId"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(*m.Body), &env) != nil {
				continue
			}
			if env.EventType == "WagerTransactionProcessed" && env.Data.ExternalTransactionID == betExt {
				_, _ = client.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{
					QueueUrl: aws.String(queue), ReceiptHandle: m.ReceiptHandle,
				})
				return true
			}
		}
		return false
	}, "bet event never reached integration-events.fifo")
}

// TestJourneyHTTPContractIsHonest pins the surface documented in the README.
// Its job is to fail loudly when prose and code drift apart — exactly the class
// of bug that produced a wrong reconciliation path in the docs.
func TestJourneyHTTPContractIsHonest(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	provider := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "10.00")

	extID := "contract-" + uuid.NewString()[:8]
	status, body := submitTransaction(t, provider, betBody(providerAClient, extID, player, walletID, "1.00"), "pa:"+extID)
	if status != http.StatusOK {
		t.Fatalf("bet: %d %v", status, body)
	}
	txID, _ := body["transactionId"].(string)

	cases := []struct {
		name   string
		method string
		path   string
		bearer string
		want   int
	}{
		{"open wallet (duplicate → conflict)", "POST", "/wallets", internal, http.StatusConflict},
		{"get wallet", "GET", "/wallets/" + walletID, internal, http.StatusOK},
		{"list ledger", "GET", "/wallets/" + walletID + "/ledger", internal, http.StatusOK},
		{"reconciliation", "POST", "/wallets/" + walletID + "/reconciliation", internal, http.StatusOK},
		{"get transaction", "GET", "/wagering/transactions/" + txID, internal, http.StatusOK},
		{"get provider transaction", "GET", "/providers/" + providerAClient + "/wagering/transactions/" + extID, provider, http.StatusOK},
		{"unknown route", "GET", "/definitely/not/a/route", internal, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if tc.method == "POST" && tc.path == "/wallets" {
				// Re-open the same player+currency to exercise the conflict path.
				payload = map[string]any{
					"playerId":       player,
					"initialBalance": map[string]string{"amount": "10.00", "currency": "BRL"},
				}
			}
			got, resp := api(t, tc.method, tc.path, tc.bearer, payload)
			if got != tc.want {
				t.Fatalf("%s %s = %d, want %d (%v)", tc.method, tc.path, got, tc.want, resp)
			}
		})
	}
}

// TestJourneyHealthAndMetricsArePublic asserts the operational surface needs no
// token — the probes used by compose and by any orchestrator.
func TestJourneyHealthAndMetricsArePublic(t *testing.T) {
	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		resp, err := http.Get(apiURL(t) + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}

	// A business route without a token must be rejected, not served.
	resp, err := http.Get(apiURL(t) + "/wallets/does-not-matter")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated business route = %d, want 401", resp.StatusCode)
	}
}
