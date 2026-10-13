//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// Challenge §13, scenario 5 — the one mandatory scenario that had no automated
// harness (ARCHITECTURE.md §11 admitted it):
//
//	"Interrompa um consumidor depois do commit e antes da remoção da mensagem."
//
// The dangerous window is exactly this: the SQL transaction that wrote the
// balance, the ledger entry, the inbox row and the outbox event has COMMITTED,
// but DeleteMessage has not run. The message's visibility timeout therefore
// expires and SQS redelivers it. Correctness then rests entirely on the durable
// inbox: the redelivery must be recognized as a duplicate, must NOT move money
// a second time, and must be acked on the second pass.
//
// A naive test would race the harness against the wall clock. This one does not:
// a wrapping TxManager signals the exact instant WithinTx returns (i.e. the
// commit is durable and the caller is about to ack) and cancels the consumer
// context there. After that, the delete can never succeed — a deterministic
// reproduction of "process died between commit and delete", without depending on
// docker kill timing or on a SIGKILL reaching a child process.

// commitGateTxManager wraps the real store and, on the first committed
// transaction, fires a one-shot channel and then blocks until the test releases
// it. Blocking after the signal is what turns "context cancelled a moment later"
// into a guarantee: the consumer cannot reach its DeleteMessage call before the
// test has cancelled the context.
type commitGateTxManager struct {
	inner *postgres.Store

	once      sync.Once
	committed chan struct{} // closed after the first successful commit
	release   chan struct{} // test closes this to unblock the handler
}

var _ service.TxManager = (*commitGateTxManager)(nil)

func (g *commitGateTxManager) Repos() service.Repos { return g.inner.Repos() }

func (g *commitGateTxManager) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	err := g.inner.WithinTx(ctx, fn)
	if err == nil {
		// Commit is durable. Signal, then hold the goroutine inside WithinTx so
		// the caller has no chance to delete the message yet.
		g.once.Do(func() {
			close(g.committed)
			<-g.release
		})
	}
	return err
}

// consumerName mirrors messaging.consumerName (unexported there): the inbox
// identity that every durable dedup row is keyed by.
const consumerName = "sqs-wager-consumer"

// inboxRows counts the durable dedup rows for one consumer identity. The inbox
// is append-only with (consumer_name, message_id) as primary key, so this is
// the row that decides whether a redelivery is a duplicate.
func inboxRows(t *testing.T, messageID string) int {
	t.Helper()
	var n int
	err := mustDB(t).QueryRow(context.Background(),
		`SELECT COUNT(*) FROM consumer_inbox WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID).Scan(&n)
	require.NoError(t, err)
	return n
}

// ledgerEntriesFor counts movements that a single external transaction wrote.
// Exactly one is correct: the retry must not append a second one.
func ledgerEntriesFor(t *testing.T, externalID string) int {
	t.Helper()
	var n int
	err := mustDB(t).QueryRow(context.Background(), `
		SELECT COUNT(*) FROM wallet_ledger_entries l
		JOIN wager_transactions t ON t.id = l.transaction_id
		WHERE t.external_transaction_id = $1`, externalID).Scan(&n)
	require.NoError(t, err)
	return n
}

// TestKillAfterCommitBeforeDelete proves scenario 5 end to end:
//
//  1. a BET is delivered; the handler commits balance + ledger + inbox + outbox;
//  2. the process "dies" — its context is cancelled between commit and delete,
//     so DeleteMessage never happens;
//  3. the visibility timeout expires and SQS redelivers the same message;
//  4. a fresh consumer instance picks it up and must recognise the duplicate,
//     keep the money unchanged, and ack.
func TestKillAfterCommitBeforeDelete(t *testing.T) {
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-kill-commit")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSMaxMessages = 10
	cfg.SQSVisibility = 2       // short: the redelivery is observable within the test
	cfg.ConsumerConcurrency = 1 // one handler: the gate must not be raced by siblings
	cfg.SQSMaxReceive = 5

	store := newTestStore(t)
	svc := newTestWagerService(t, store, 3, time.Minute)

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	extID := "ext-" + uuid.NewString()[:8]
	messageID := uuid.NewString()

	body, err := json.Marshal(envelope(messageID, "BET",
		wagerData("provider-a", extID, player, walletID, "BET", "25.00")))
	require.NoError(t, err)

	// --- instance #1: commits, then dies before the delete --------------------

	gate := &commitGateTxManager{inner: store, committed: make(chan struct{}), release: make(chan struct{})}
	h1 := &consumerHarness{
		client: client,
		urls:   sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL},
		cfg:    cfg,
		store:  store,
		svc:    svc,
		hook:   &recordingMetrics{},
	}
	h1.cons = messaging.NewConsumer(client, h1.urls, cfg, gate, svc, h1.hook, nil, testLogger)

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = client.SendMessage(sendCtx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(rootURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String("test-group"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	sendCancel()
	require.NoError(t, err, "send the BET")

	runCtx, killConsumer := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = h1.cons.Run(runCtx)
	}()

	// Wait for the durable commit of *this* message, then kill the process
	// before it is allowed to delete. The gate holds the handler inside
	// WithinTx, so the cancellation lands in the exact intra-window.
	select {
	case <-gate.committed:
	case <-time.After(30 * time.Second):
		killConsumer()
		close(gate.release)
		<-loopDone
		t.Fatal("the first instance never committed the transaction")
	}

	// The commit is durable and visible before the delete is even attempted.
	require.Eventually(t, func() bool {
		var status string
		err := mustDB(t).QueryRow(context.Background(),
			`SELECT status FROM wager_transactions WHERE external_transaction_id = $1`, extID).Scan(&status)
		return err == nil && status == "PROCESSED"
	}, 10*time.Second, 100*time.Millisecond, "the transaction must be committed as PROCESSED")

	balanceAfterFirst := walletBalanceDirect(t, walletID)
	require.Equal(t, "75.00", balanceAfterFirst, "100.00 opening - 25.00 BET")

	// Kill: cancel the consumer context and release the gate. The handler now
	// unwinds with a cancelled context, so DeleteMessage is never issued.
	killConsumer()
	close(gate.release)

	select {
	case <-loopDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the killed consumer loop did not return")
	}

	// The message was committed but never deleted: precisely the precondition of
	// scenario 5. Its ledger movement must exist exactly once, and the inbox row
	// is what a redelivery will match against.
	assert.Equal(t, 1, ledgerEntriesFor(t, extID), "exactly one ledger movement after the commit")
	assert.Equal(t, 1, inboxRows(t, messageID), "the committed inbox row is the dedup anchor")

	// --- the redelivery -------------------------------------------------------
	//
	// A fresh consumer, with the real store (the gate is gone), now sees the same
	// message again. It must treat it as a duplicate: no second debit, no second
	// ledger entry, and the message must be acked rather than retried or poisoned.
	hook2 := &recordingMetrics{}
	h2 := &consumerHarness{
		client: client,
		urls:   sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL},
		cfg:    cfg,
		store:  store,
		svc:    svc,
		hook:   hook2,
	}
	h2.cons = messaging.NewConsumer(client, h2.urls, cfg, store, svc, hook2, nil, testLogger)
	h2.run(t)

	require.Eventually(t, func() bool {
		return hook2.dupCount() >= 1
	}, 30*time.Second, 250*time.Millisecond,
		"the redelivery must be recognised as a duplicate via the durable inbox")

	// Money moved once and only once, whatever the redelivery did.
	assert.Equal(t, "75.00", walletBalanceDirect(t, walletID),
		"the redelivery must not debit a second time")
	assert.Equal(t, 1, ledgerEntriesFor(t, extID),
		"the redelivery must not append a second ledger entry")
	assert.Zero(t, hook2.dlqCount(), "a duplicate is not poison: it must not reach the DLQ")

	// The inbox stayed append-only: a duplicate is not recorded as a second row.
	assert.Equal(t, 1, inboxRows(t, messageID), "inbox row count must not grow on redelivery")

	// The challenge's closing instruction: stored balance == ledger sum. Checked
	// on the wallet this test owns, under a consistent snapshot.
	assert.Equal(t, int64(7500), reconcileWallet(t, walletID),
		"100.00 opening - 25.00 BET, counted once")

	// The queue drains: the duplicate was acked, not left to redeliver forever.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := client.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:            aws.String(rootURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		return err == nil && len(out.Messages) == 0
	}, 45*time.Second, 500*time.Millisecond,
		"after the duplicate is acked the queue must be empty")
}

// reconcileWallet checks the accounting identity for ONE wallet — the challenge's
// closing instruction ("confira o saldo armazenado contra a soma de créditos menos
// débitos do ledger") — inside a REPEATABLE READ transaction so both SELECTs see
// the same snapshot. A non-transactional check over every wallet is flaky here by
// construction: other tests open wallets concurrently and a reader would observe
// the wallets row before, or the ledger rows after, the writer's commit.
//
// REPEATABLE READ is enough: the two reads are consistent with each other, which
// is exactly what the stored-vs-ledger identity claims.
func reconcileWallet(t *testing.T, walletID string) int64 {
	t.Helper()
	ctx := context.Background()

	pool := mustDB(t)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	var stored int64
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT balance_minor FROM wallets WHERE id = $1::uuid`, walletID).Scan(&stored))

	var ledger int64
	require.NoError(t, tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor
		                         ELSE -amount_minor END), 0)
		FROM wallet_ledger_entries WHERE wallet_id = $1::uuid`, walletID).Scan(&ledger))

	assert.Equal(t, ledger, stored,
		"wallet %s: stored balance must equal the ledger sum", walletID)
	return stored
}
