//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestJournalIsWrittenAndBalanced proves the double-entry layer against a real
// database: a wallet movement must leave a journal entry whose debits equal its
// credits, and the player leg must match the ledger row exactly.
//
// The unit tests assert the service calls Postings().Append; only this test
// asserts the rows actually exist and balance, which is the property the
// challenge rewards.
func TestJournalIsWrittenAndBalanced(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	providerA := token(t, providerAClient, providerASecret)

	// The opening itself is a movement: it must have a journal entry too.
	assertJournalBalances(t, ctx, pool, walletID, "OPENING")
	assertPlayerLegMatchesLedger(t, ctx, pool, walletID)

	extID := "journal-" + uuid.NewString()[:8]
	if st, body := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "30.00"), "provider-a:"+extID); st != 200 {
		t.Fatalf("bet: %d %v", st, body)
	}

	assertJournalBalances(t, ctx, pool, walletID, "BET")
	assertPlayerLegMatchesLedger(t, ctx, pool, walletID)

	// The BET's entry is exactly two legs, and a BET debits the player while
	// crediting the house.
	var debits, credits int64
	if err := pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'DEBIT'), 0),
			COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'CREDIT'), 0)
		FROM postings p
		JOIN wager_transactions t ON t.id = p.transaction_id
		WHERE t.kind = 'BET' AND t.wallet_id = $1`, walletID).Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits || debits != 3000 {
		t.Fatalf("BET journal: debits %d, credits %d, want 3000/3000", debits, credits)
	}
}

// TestJournalRefusesDivergentPlayerLeg proves the database trigger, not just
// the application: a player posting that disagrees with the committed ledger
// row is rejected. This is what stops the journal from silently becoming a
// second, contradicting source of truth.
func TestJournalRefusesDivergentPlayerLeg(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "100.00")
	providerA := token(t, providerAClient, providerASecret)

	extID := "divergent-" + uuid.NewString()[:8]
	if st, body := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "30.00"), "provider-a:"+extID); st != 200 {
		t.Fatalf("bet: %d %v", st, body)
	}

	var txID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM wager_transactions WHERE wallet_id = $1 AND kind = 'BET' LIMIT 1`, walletID).
		Scan(&txID); err != nil {
		t.Fatal(err)
	}

	// A player posting for a real transaction but with the WRONG amount: the
	// trigger must reject it, because no ledger row says the player moved this.
	_, err := pool.Exec(ctx, `
		INSERT INTO postings (id, transaction_id, account_kind, account_ref, direction, currency, amount_minor)
		VALUES (gen_random_uuid(), $1, 'PLAYER_WALLET', $2, 'DEBIT', 'BRL', 999999)`, txID, walletID)
	if err == nil {
		t.Fatal("the trigger accepted a player posting with no matching ledger row")
	}
	t.Logf("rejected as expected: %v", err)

	// And the append-only triggers: the journal cannot be edited or erased.
	_, err = pool.Exec(ctx, `UPDATE postings SET amount_minor = amount_minor + 1 WHERE transaction_id = $1`, txID)
	if err == nil {
		t.Fatal("postings accepted an UPDATE; the journal is supposed to be append-only")
	}
	_, err = pool.Exec(ctx, `DELETE FROM postings WHERE transaction_id = $1`, txID)
	if err == nil {
		t.Fatal("postings accepted a DELETE; the journal is supposed to be append-only")
	}
}

// TestRejectedMovementLeavesNoJournal is the rollback half of the double-entry
// claim, and it can only be proven against a real database: the fake TxManager
// in the unit tier has no transaction semantics at all (its WithinTx runs the
// callback inline), so a unit assertion would be meaningless. Here WithinTx is
// pgx.BeginFunc, and pgx discards every write the callback performed if it
// returns an error.
//
// The rejected path is the one that matters: the use case locks the wallet and
// begins work before it discovers the movement is impossible. A rejected BET
// must leave no journal entry, no ledger row and an unchanged balance — a
// movement commits with its counterparty leg or does not commit at all.
func TestRejectedMovementLeavesNoJournal(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "10.00")
	providerA := token(t, providerAClient, providerASecret)

	before := countJournal(t, ctx, pool, walletID)

	// A BET larger than the balance is rejected (422); no journal entry, no
	// ledger row, and the balance is untouched.
	extID := "reject-" + uuid.NewString()[:8]
	if st, _ := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "999.00"), "provider-a:"+extID); st != 422 {
		t.Fatalf("expected 422 INSUFFICIENT_FUNDS, got %d", st)
	}

	if after := countJournal(t, ctx, pool, walletID); after != before {
		t.Fatalf("rejected bet changed the journal: %d -> %d", before, after)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "10.00" {
		t.Fatalf("rejected bet moved the balance: %s", bal)
	}
}

// --- helpers ----------------------------------------------------------------

// assertJournalBalances asserts that a transaction of the given kind on this
// wallet has a journal entry, and that its debits equal its credits.
func assertJournalBalances(t *testing.T, ctx context.Context, pool *pgxpool.Pool, walletID, kind string) {
	t.Helper()
	var debits, credits int64
	if err := pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'DEBIT'), 0),
			COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'CREDIT'), 0)
		FROM postings p
		JOIN wager_transactions tx ON tx.id = p.transaction_id
		WHERE tx.wallet_id = $1 AND tx.kind = $2`, walletID, kind).Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits == 0 && credits == 0 {
		t.Fatalf("no journal entry for %s on wallet %s", kind, walletID)
	}
	if debits != credits {
		t.Fatalf("%s journal does not balance: debits %d, credits %d", kind, debits, credits)
	}
}

// assertPlayerLegMatchesLedger asserts the player's posting for every
// transaction on this wallet has the same direction and amount as the ledger
// row — the cross-table invariant the trigger enforces on write.
func assertPlayerLegMatchesLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, walletID string) {
	t.Helper()
	var mismatches int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM wallet_ledger_entries l
		LEFT JOIN postings p
			ON p.transaction_id = l.transaction_id
			AND p.account_kind = 'PLAYER_WALLET'
			AND p.account_ref = l.wallet_id::text
		WHERE l.wallet_id = $1
		  AND (p.id IS NULL
		       OR p.direction <> l.direction
		       OR p.amount_minor <> l.amount_minor)`, walletID).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("%d ledger rows disagree with their player posting", mismatches)
	}
}

func countJournal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, walletID string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM postings p
		JOIN wager_transactions tx ON tx.id = p.transaction_id
		WHERE tx.wallet_id = $1`, walletID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
