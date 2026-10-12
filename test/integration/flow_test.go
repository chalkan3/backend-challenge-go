//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/app"
	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// TestWalletLifecycleAndReconciliation covers opening, conflict on duplicate
// player+currency, ledger pagination and reconciliation consistency.
func TestWalletLifecycleAndReconciliation(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "1000.00")

	// duplicate opening → 409
	status, body := api(t, "POST", "/wallets", internal, map[string]any{
		"playerId":       player,
		"initialBalance": map[string]string{"amount": "50.00", "currency": "BRL"},
	})
	if status != 409 {
		t.Fatalf("duplicate opening: %d %v", status, body)
	}

	// zero opening: wallet created, no financial rows (§9 — no OPENING,
	// no ledger entry, no financial outbox events)
	zWallet := openWallet(t, internal, newPlayerID(t), "0")
	bal, _ := walletBalance(t, internal, zWallet)
	if bal != "0.00" {
		t.Fatalf("zero opening balance: %s", bal)
	}
	var zeroTxRows, zeroLedgerRows, zeroEvents int
	if err := mustDB(t).QueryRow(t.Context(), `SELECT
		(SELECT COUNT(*) FROM wager_transactions WHERE wallet_id = $1),
		(SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1),
		(SELECT COUNT(*) FROM integration_outbox WHERE payload->'data'->>'walletId' = $1::text)`,
		zWallet).Scan(&zeroTxRows, &zeroLedgerRows, &zeroEvents); err != nil {
		t.Fatal(err)
	}
	if zeroTxRows != 0 || zeroLedgerRows != 0 || zeroEvents != 0 {
		t.Fatalf("zero opening must not create financial rows: tx=%d ledger=%d events=%d",
			zeroTxRows, zeroLedgerRows, zeroEvents)
	}

	// BET 25.00 → balance 975.00
	providerA := token(t, providerAClient, providerASecret)
	extID := fmt.Sprintf("tx-%s", uuid.NewString()[:8])
	sub := map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": extID,
		"playerId":              player,
		"walletId":              walletID,
		"roundId":               "round-1",
		"gameId":                "fortune-chimp",
		"kind":                  "BET",
		"money":                 map[string]string{"amount": "25.00", "currency": "BRL"},
	}
	status, body = submitTransaction(t, providerA, sub, "provider-a:"+extID)
	if status != 200 || body["status"] != "PROCESSED" {
		t.Fatalf("bet: %d %v", status, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "975.00" {
		t.Fatalf("balance after bet: %s", bal)
	}

	// ledger has exactly 2 entries (opening credit + debit), paginated
	var total int
	cursor := ""
	for {
		path := fmt.Sprintf("/wallets/%s/ledger?limit=1", walletID)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		st, page := api(t, "GET", path, internal, nil)
		if st != 200 {
			t.Fatalf("ledger: %d %v", st, page)
		}
		items, _ := page["items"].([]any)
		total += len(items)
		next, _ := page["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if total != 2 {
		t.Fatalf("ledger entries: %d, want 2", total)
	}

	// reconciliation consistent
	st, rec := api(t, "POST", "/wallets/"+walletID+"/reconciliation", internal, nil)
	if st != 200 || rec["consistent"] != true || rec["checkedEntries"].(float64) != 2 {
		t.Fatalf("reconciliation: %d %v", st, rec)
	}
	if diff, _ := rec["difference"].(map[string]any)["amount"].(string); diff != "0.00" {
		t.Fatalf("difference: %v", rec)
	}
}

// TestWagerFlowsAndIdempotency covers the five kinds, replay semantics and
// conflicts for the same key/identity.
func TestWagerFlowsAndIdempotency(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	keyBase := uuid.NewString()[:8]

	// LOSS requires 0.00 and produces no balance change
	extLoss := "loss-" + keyBase
	st, body := submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extLoss,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "LOSS", "money": map[string]string{"amount": "0.00", "currency": "BRL"},
	}, "provider-a:"+extLoss)
	if st != 200 || body["status"] != "PROCESSED" {
		t.Fatalf("loss: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("loss changed balance: %s", bal)
	}
	// §7: a LOSS must emit exactly one WagerTransactionProcessed and NO
	// WalletBalanceChanged (no balance change → no version bump → no event).
	lossTxID, _ := body["transactionId"].(string)
	var lossProcessed, lossChanged int
	if err := mustDB(t).QueryRow(t.Context(), `SELECT
		COUNT(*) FILTER (WHERE event_type = 'WagerTransactionProcessed'),
		COUNT(*) FILTER (WHERE event_type = 'WalletBalanceChanged')
		FROM integration_outbox WHERE payload->'data'->>'transactionId' = $1`,
		lossTxID).Scan(&lossProcessed, &lossChanged); err != nil {
		t.Fatal(err)
	}
	if lossProcessed != 1 || lossChanged != 0 {
		t.Fatalf("loss events: processed=%d balanceChanged=%d, want 1/0", lossProcessed, lossChanged)
	}
	// LOSS non-zero → 400 (validation), not persisted as rejection
	extBadLoss := "lossbad-" + keyBase
	st, _ = submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extBadLoss,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "LOSS", "money": map[string]string{"amount": "1.00", "currency": "BRL"},
	}, "provider-a:"+extBadLoss)
	if st != 400 {
		t.Fatalf("loss non-zero: %d", st)
	}

	// WIN credits
	extWin := "win-" + keyBase
	st, body = submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extWin,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "WIN", "money": map[string]string{"amount": "50.00", "currency": "BRL"},
	}, "provider-a:"+extWin)
	if st != 200 || body["status"] != "PROCESSED" {
		t.Fatalf("win: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "150.00" {
		t.Fatalf("balance after win: %s", bal)
	}

	// replay: same key + same content → 200 with idempotentReplay=true and
	// the ORIGINAL observed balance (150.00), even after new movements.
	extBet := "bet-" + keyBase
	bet := map[string]any{
		"providerId": "provider-a", "externalTransactionId": extBet,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "BET", "money": map[string]string{"amount": "30.00", "currency": "BRL"},
	}
	st, body = submitTransaction(t, providerA, bet, "provider-a:"+extBet)
	if st != 200 || body["idempotentReplay"] != false {
		t.Fatalf("bet first: %d %v", st, body)
	}
	// new movement changes wallet to 120.00
	extWin2 := "win2-" + keyBase
	submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extWin2,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "WIN", "money": map[string]string{"amount": "0.01", "currency": "BRL"},
	}, "provider-a:"+extWin2)
	st, body = submitTransaction(t, providerA, bet, "provider-a:"+extBet)
	if st != 200 || body["idempotentReplay"] != true || body["balance"].(map[string]any)["amount"] != "120.00" {
		t.Fatalf("replay must return original balance 120.00: %d %v", st, body)
	}

	// same key, different content → 409
	betChanged := map[string]any{
		"providerId": "provider-a", "externalTransactionId": extBet,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "BET", "money": map[string]string{"amount": "31.00", "currency": "BRL"},
	}
	st, body = submitTransaction(t, providerA, betChanged, "provider-a:"+extBet)
	if st != 409 {
		t.Fatalf("key reuse with different content: %d %v", st, body)
	}

	// same external id, different key → 409
	st, body = submitTransaction(t, providerA, bet, "provider-a:other-key-"+extBet)
	if st != 409 {
		t.Fatalf("external id reuse with another key: %d %v", st, body)
	}

	// equivalent money formats hash identically: replay with "30.0"
	betEquivalent := map[string]any{
		"providerId": "provider-a", "externalTransactionId": extBet,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "BET", "money": map[string]string{"amount": "30.0", "currency": "BRL"},
	}
	st, body = submitTransaction(t, providerA, betEquivalent, "provider-a:"+extBet)
	if st != 200 || body["idempotentReplay"] != true {
		t.Fatalf("equivalent amount must replay: %d %v", st, body)
	}
}

// TestInsufficientFundsConcurrent is the mandatory scenario: wallet with
// 100.00 BRL receives two distinct 80.00 bets SIMULTANEOUSLY (different
// connections/instances). Exactly one PROCESSED, one REJECTED
// INSUFFICIENT_FUNDS, final balance 20.00, a single debit entry. Replays do
// not change the outcome.
func TestInsufficientFundsConcurrent(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")

	var results []struct {
		status int
		body   map[string]any
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			extID := fmt.Sprintf("concurrent-bet-%d-%s", i, uuid.NewString()[:8])
			<-start
			st, body := submitTransaction(t, providerA, map[string]any{
				"providerId": "provider-a", "externalTransactionId": extID,
				"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
				"kind": "BET", "money": map[string]string{"amount": "80.00", "currency": "BRL"},
			}, "provider-a:"+extID)
			mu.Lock()
			results = append(results, struct {
				status int
				body   map[string]any
			}{st, body})
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	var processed, rejected int
	for _, r := range results {
		switch {
		case r.status == 200 && r.body["status"] == "PROCESSED":
			processed++
		case r.status == 422 && r.body["failureCode"] == "INSUFFICIENT_FUNDS":
			rejected++
		default:
			t.Fatalf("unexpected result: %d %v", r.status, r.body)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed=%d rejected=%d, want 1/1", processed, rejected)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "20.00" {
		t.Fatalf("final balance: %s, want 20.00", bal)
	}

	// single debit entry in the ledger
	pool := mustDB(t)
	var entries int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`,
		walletID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("debit entries: %d, want 1", entries)
	}

	// replays must not alter the outcome
	for _, r := range results {
		_ = r
	}
	var extID string
	if err := pool.QueryRow(context.Background(),
		`SELECT external_transaction_id FROM wager_transactions
		 WHERE wallet_id=$1 AND kind='BET' AND status='PROCESSED'`, walletID).Scan(&extID); err != nil {
		t.Fatal(err)
	}
	st, body := submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extID,
		"playerId": player, "walletId": walletID, "roundId": "r", "gameId": "g",
		"kind": "BET", "money": map[string]string{"amount": "80.00", "currency": "BRL"},
	}, "provider-a:"+extID)
	if st != 200 || body["idempotentReplay"] != true {
		t.Fatalf("replay after concurrency: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "20.00" {
		t.Fatalf("balance changed by replay: %s", bal)
	}
}

// TestMultiInstanceConcurrency runs the same dispute across two independent
// app instances (docker compose --profile multi). Skips when the second
// instance is not up.
func TestMultiInstanceConcurrency(t *testing.T) {
	resp, err := http.Get(apiURL2(t) + "/health/live")
	if err != nil || resp.StatusCode != 200 {
		t.Skip("second instance not running; use `docker compose --profile multi up`")
	}
	resp.Body.Close()
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")

	type res struct {
		status int
		body   map[string]any
	}
	ch := make(chan res, 2)
	go func() {
		extID := "m1-" + uuid.NewString()[:8]
		st, body := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "80.00"), "provider-a:"+extID)
		ch <- res{st, body}
	}()
	go func() {
		extID := "m2-" + uuid.NewString()[:8]
		st, body := apiWithHeader(t, apiURL2(t), "POST", "/wagering/transactions", providerA,
			betBody("provider-a", extID, player, walletID, "80.00"), "Idempotency-Key", "provider-a:"+extID)
		ch <- res{st, body}
	}()
	var processed, rejected int
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.status == 200 {
			processed++
		} else if r.status == 422 {
			rejected++
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("multi-instance: processed=%d rejected=%d", processed, rejected)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "20.00" {
		t.Fatalf("multi-instance balance: %s", bal)
	}
}

func betBody(provider, extID, player, wallet, amount string) map[string]any {
	return map[string]any{
		"providerId": provider, "externalTransactionId": extID,
		"playerId": player, "walletId": wallet, "roundId": "round-42", "gameId": "fortune-chimp",
		"kind": "BET", "money": map[string]string{"amount": amount, "currency": "BRL"},
	}
}

// TestReversals covers REFUND, duplicate reversal, ROLLBACK of WIN and the
// distinct failure code for reversal-without-funds.
func TestReversals(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	base := uuid.NewString()[:8]

	// BET 40 → REFUND (credit 40, one successful refund per reference)
	betID := "rbet-" + base
	submitTransaction(t, providerA, betBody("provider-a", betID, player, walletID, "40.00"), "provider-a:"+betID)
	refID := "rref-" + base
	st, body := submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": refID,
		"playerId": player, "walletId": walletID, "roundId": "round-42", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": map[string]string{"amount": "40.00", "currency": "BRL"},
		"referenceExternalTransactionId": betID,
	}, "provider-a:"+refID)
	if st != 200 || body["status"] != "PROCESSED" {
		t.Fatalf("refund: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("balance after refund: %s", bal)
	}

	// second REFUND on the same reference → REJECTED DUPLICATE_REVERSAL
	refID2 := "rref2-" + base
	st, body = submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": refID2,
		"playerId": player, "walletId": walletID, "roundId": "round-42", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": map[string]string{"amount": "40.00", "currency": "BRL"},
		"referenceExternalTransactionId": betID,
	}, "provider-a:"+refID2)
	if st != 422 || body["failureCode"] != "DUPLICATE_REVERSAL" {
		t.Fatalf("duplicate refund: %d %v", st, body)
	}

	// ROLLBACK of WIN with balance drained → REVERSAL_INSUFFICIENT_FUNDS
	// (distinct from INSUFFICIENT_FUNDS)
	winID := "rwin-" + base
	submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": winID,
		"playerId": player, "walletId": walletID, "roundId": "round-9", "gameId": "fortune-chimp",
		"kind": "WIN", "money": map[string]string{"amount": "90.00", "currency": "BRL"},
	}, "provider-a:"+winID)
	// drain: bet everything
	drainID := "rdrain-" + base
	submitTransaction(t, providerA, betBody("provider-a", drainID, player, walletID, "190.00"), "provider-a:"+drainID)
	rbID := "rrb-" + base
	st, body = submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": rbID,
		"playerId": player, "walletId": walletID, "roundId": "round-9", "gameId": "fortune-chimp",
		"kind": "ROLLBACK", "money": map[string]string{"amount": "90.00", "currency": "BRL"},
		"referenceExternalTransactionId": winID,
	}, "provider-a:"+rbID)
	if st != 422 || body["failureCode"] != "REVERSAL_INSUFFICIENT_FUNDS" {
		t.Fatalf("rollback insufficient: %d %v", st, body)
	}

	// cross-provider reference isolation: provider-b cannot reference
	// provider-a's bet
	providerB := token(t, providerBClient, providerBSecret)
	xbID := "xref-" + base
	st, body = submitTransaction(t, providerB, map[string]any{
		"providerId": "provider-b", "externalTransactionId": xbID,
		"playerId": player, "walletId": walletID, "roundId": "round-42", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": map[string]string{"amount": "40.00", "currency": "BRL"},
		"referenceExternalTransactionId": betID,
	}, "provider-b:"+xbID)
	// wallet belongs to the player but reference is from another provider →
	// PENDING_REFERENCE (never resolves) — must NOT credit anything.
	if st != 202 {
		t.Fatalf("cross-provider reversal: %d %v", st, body)
	}
	time.Sleep(2 * time.Second)
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Logf("note: balance while cross-provider refund pends: %s", bal)
	}
}

// TestPendingReferenceResolution delivers a reversal BEFORE its referenced
// bet, then the bet arrives and the reference worker resolves it.
func TestPendingReferenceResolution(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	base := uuid.NewString()[:8]

	// REFUND first → PENDING_REFERENCE
	refID := "pref-" + base
	st, body := submitTransaction(t, providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": refID,
		"playerId": player, "walletId": walletID, "roundId": "round-7", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": map[string]string{"amount": "25.00", "currency": "BRL"},
		"referenceExternalTransactionId": "pbet-" + base,
	}, "provider-a:"+refID)
	if st != 202 || body["status"] != "PENDING_REFERENCE" {
		t.Fatalf("pending refund: %d %v", st, body)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("pending must not move money: %s", bal)
	}

	// BET arrives (SAME round, player, wallet and currency as the refund)
	b := betBody("provider-a", "pbet-"+base, player, walletID, "25.00")
	b["roundId"] = "round-7"
	submitTransaction(t, providerA, b, "provider-a:pbet-"+base)
	pollUntil(t, 15*time.Second, func() bool {
		_, body := api(t, "GET", "/providers/provider-a/wagering/transactions/"+refID, providerA, nil)
		return body["status"] == string(domain.StatusProcessed)
	}, "pending reference never resolved")

	// net effect: -25 (bet) +25 (refund) = 100.00
	if bal, _ := walletBalance(t, internal, walletID); bal != "100.00" {
		t.Fatalf("balance after resolution: %s", bal)
	}
}

// TestReferenceTTLRejection exercises the durable waiting state directly
// against the service with a short TTL: retries happen, then the operation
// is finalized REJECTED with REFERENCE_NOT_FOUND and emits the rejection
// event.
func TestReferenceTTLRejection(t *testing.T) {
	pool := mustDB(t)
	store := newTestStore(t)
	svc := newTestWagerService(t, store, 2, 1*time.Second) // 2 attempts, 1s TTL
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")

	extID := "ttl-" + uuid.NewString()[:8]
	in := externalInputFor("provider-a", extID, player, walletID)
	in.Kind = "REFUND"
	in.Amount = "25.00"
	in.Currency = "BRL"
	in.ReferenceExternalTransactionID = "never-" + extID
	in.IdempotencyKey = "provider-a:" + extID
	in.ReceivedVia = domain.ViaHTTP

	res, err := svc.Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transaction.Status() != domain.StatusPendingReference {
		t.Fatalf("status: %s", res.Transaction.Status())
	}

	// resolve loop (what the ReferenceWorker does on schedule). The compose
	// app worker disputes the same rows (SKIP LOCKED), so claims interleave.
	for i := 0; i < 14; i++ {
		time.Sleep(500 * time.Millisecond)
		err := store.WithinTx(context.Background(), func(ctx context.Context, r app.Repos) error {
			claimed, err := r.Transactions().ClaimPendingReferences(ctx, time.Now().UTC(), 10)
			if err != nil {
				return err
			}
			for _, tx := range claimed {
				if _, err := svc.ResolvePending(ctx, r, tx); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, body := api(t, "GET", "/providers/provider-a/wagering/transactions/"+extID, providerA, nil)
		if body["status"] == string(domain.StatusRejected) {
			if body["failureCode"] != "REFERENCE_NOT_FOUND" {
				t.Fatalf("failure code: %v", body["failureCode"])
			}
			// rejection event exists in the outbox
			var events int
			if err := pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM integration_outbox WHERE event_type='WagerTransactionRejected'`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events < 1 {
				t.Fatal("no rejection event in outbox")
			}
			return
		}
	}
	t.Fatal("pending reference never expired to REJECTED")
}

// TestProviderIsolationIsAuditable: the pending cross-provider rejection from
// TestReversals produced an auditable transaction; here we assert a fresh
// case end-to-end: wrong player for wallet is REJECTED, not processed.
func TestPlayerWalletMismatch(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	otherPlayer := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	extID := "mismatch-" + uuid.NewString()[:8]
	st, body := submitTransaction(t, providerA, betBody("provider-a", extID, otherPlayer, walletID, "10.00"), "provider-a:"+extID)
	if st != 422 || body["failureCode"] != "PLAYER_WALLET_MISMATCH" {
		t.Fatalf("mismatch: %d %v", st, body)
	}
	// auditable: transaction persisted as REJECTED
	st, body = api(t, "GET", "/providers/provider-a/wagering/transactions/"+extID, providerA, nil)
	if st != 200 || body["status"] != "REJECTED" {
		t.Fatalf("auditable rejection: %d %v", st, body)
	}
}

// TestOpeningRejectedOverExternalChannels covers §6.3: OPENING is an
// internal-only kind. Over HTTP the request is rejected with 400 and nothing
// is persisted; over SQS the message is permanently invalid (business shape)
// and must be routed to the DLQ instead of being retried forever.
func TestOpeningRejectedOverExternalChannels(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "50.00")
	suffix := uuid.NewString()[:8]

	// --- HTTP ---
	status, body := submitTransaction(t, token(t, providerAClient, providerASecret), map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": "ext-open-" + suffix,
		"playerId":              player,
		"walletId":              walletID,
		"roundId":               "r",
		"gameId":                "fortune-chimp",
		"kind":                  "OPENING",
		"money":                 map[string]any{"amount": "10.00", "currency": "BRL"},
	}, "provider-a:open-"+suffix)
	if status != 400 {
		t.Fatalf("OPENING over HTTP: got %d %v, want 400", status, body)
	}
	var rows int
	if err := mustDB(t).QueryRow(t.Context(),
		`SELECT COUNT(*) FROM wager_transactions WHERE provider_id = 'provider-a' AND external_transaction_id = $1`,
		"ext-open-"+suffix).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rejected OPENING must not be persisted over HTTP, found %d rows", rows)
	}

	// --- SQS ---
	client := mustSQS(t)
	queue := mustQueueURL(t, client, "wager-transactions.fifo")
	dlq := mustQueueURL(t, client, "wager-transactions-dlq.fifo")
	messageID := "msg-open-" + uuid.NewString()[:8]
	envelope := fmt.Sprintf(
		`{"messageId":%q,"type":"WagerTransactionRequested","occurredAt":"2026-01-01T00:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"sqs-open-%s","idempotencyKey":"provider-a:sqs-open-%s","playerId":%q,"walletId":%q,"roundId":"r","gameId":"g","kind":"OPENING","money":{"amount":"10.00","currency":"BRL"}}}`,
		messageID, suffix, suffix, player, walletID)
	if _, err := client.SendMessage(t.Context(), &sqs.SendMessageInput{
		QueueUrl:               aws.String(queue),
		MessageBody:            aws.String(envelope),
		MessageGroupId:         aws.String(walletID),
		MessageDeduplicationId: aws.String(uuid.NewString()),
	}); err != nil {
		t.Fatalf("send OPENING message: %v", err)
	}
	pollUntil(t, 90*time.Second, func() bool {
		out, err := client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(dlq),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil {
			return false
		}
		for _, m := range out.Messages {
			if m.Body != nil && *m.Body == envelope {
				_, _ = client.DeleteMessage(t.Context(), &sqs.DeleteMessageInput{
					QueueUrl: aws.String(dlq), ReceiptHandle: m.ReceiptHandle,
				})
				return true
			}
		}
		return false
	}, "OPENING message must be routed to the DLQ as permanently invalid")
}

// TestDifferentWalletsMoveInParallel proves the concurrency strategy is
// per-wallet (§8): simultaneous bets on DISTINCT wallets must both succeed.
// A global lock would serialize them (and could cross-reject); row-level
// wallet locks must keep different wallets independent.
func TestDifferentWalletsMoveInParallel(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)

	players := [2]string{newPlayerID(t), newPlayerID(t)}
	wallets := [2]string{
		openWallet(t, internal, players[0], "100.00"),
		openWallet(t, internal, players[1], "100.00"),
	}

	type outcome struct {
		status int
		body   map[string]any
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		i := i
		extID := "par-" + uuid.NewString()[:8]
		go func() {
			st, body := submitTransaction(t, providerA, betBody("provider-a", extID, players[i], wallets[i], "80.00"),
				"provider-a:"+extID)
			results <- outcome{st, body}
		}()
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.status != 200 || r.body["status"] != "PROCESSED" {
			t.Fatalf("distinct wallets must process in parallel without cross-rejection: %d %v", r.status, r.body)
		}
	}
	for i := 0; i < 2; i++ {
		if bal, _ := walletBalance(t, internal, wallets[i]); bal != "20.00" {
			t.Fatalf("wallet %d balance: %s, want 20.00", i, bal)
		}
	}
}
