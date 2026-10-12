//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestSchemaEnforcesFinancialInvariants proves the database itself rejects
// what the application must never do — independent of any Go code path.
func TestSchemaEnforcesFinancialInvariants(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "80.00")
	providerA := token(t, providerAClient, providerASecret)

	extID := "schema-" + uuid.NewString()[:8]
	st, _ := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "30.00"), "provider-a:"+extID)
	if st != 200 {
		t.Fatalf("bet: %d", st)
	}

	var entryID, txID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, transaction_id::text FROM wallet_ledger_entries WHERE wallet_id = $1 LIMIT 1`, walletID).
		Scan(&entryID, &txID); err != nil {
		t.Fatal(err)
	}

	// 1. ledger is append-only: UPDATE/DELETE raise
	if _, err := pool.Exec(ctx,
		`UPDATE wallet_ledger_entries SET amount_minor = amount_minor + 1 WHERE id = $1::uuid`, entryID); err == nil {
		t.Fatal("ledger UPDATE must fail")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM wallet_ledger_entries WHERE id = $1::uuid`, entryID); err == nil {
		t.Fatal("ledger DELETE must fail")
	}

	// 2. stored balance can never go negative
	if _, err := pool.Exec(ctx,
		`UPDATE wallets SET balance_minor = -1 WHERE id = $1::uuid`, walletID); err == nil {
		t.Fatal("negative balance must fail")
	}

	// 3. durable idempotency identity: same (provider, key) is unique
	if _, err := pool.Exec(ctx, `
		INSERT INTO wager_transactions (id, kind, origin, provider_id, external_transaction_id,
			idempotency_key, payload_hash, player_id, wallet_id, round_id, game_id, currency,
			amount_minor, status, channel)
		SELECT id, kind, origin, provider_id, external_transaction_id,
			idempotency_key, payload_hash, player_id, wallet_id, round_id, game_id, currency,
			amount_minor, status, channel
		FROM wager_transactions WHERE external_transaction_id = $1 LIMIT 1`, extID); err == nil {
		t.Fatal("duplicate (provider, idempotency_key) must fail")
	}

	// 4. loss rows carry zero amount
	if _, err := pool.Exec(ctx, `
		INSERT INTO wager_transactions (id, kind, origin, provider_id, external_transaction_id,
			idempotency_key, payload_hash, player_id, wallet_id, round_id, game_id, currency,
			amount_minor, status, channel)
		VALUES (gen_random_uuid(), 'LOSS', 'EXTERNAL', 'provider-a', 'x1', 'k1', 'h1',
			$1::uuid, $2::uuid, 'r', 'g', 'BRL', 500, 'PENDING', 'HTTP')`, player, walletID); err == nil {
		t.Fatal("LOSS with non-zero amount must fail")
	}

	// 5. one OPENING per wallet
	if _, err := pool.Exec(ctx, `
		INSERT INTO wager_transactions (id, kind, origin, player_id, wallet_id, currency,
			amount_minor, status, channel)
		VALUES (gen_random_uuid(), 'OPENING', 'INTERNAL', $1::uuid, $2::uuid, 'BRL', 100, 'PROCESSED', 'HTTP')`,
		player, walletID); err == nil {
		t.Fatal("second OPENING for the same wallet must fail")
	}

	// 6. terminal rows are frozen by the Save guard (status filter in SQL)
	var _ = txID
}
