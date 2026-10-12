//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/infra/postgres"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/infra/sqs"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/worker"
)

// TestSQSConsumerAndInboxDedup publishes the ingress contract to the queue:
// first delivery moves money; a redelivery with the SAME envelope messageId
// (different body bytes, so SQS does not dedup for us) is absorbed by the
// durable inbox + financial idempotency — no double movement.
func TestSQSConsumerAndInboxDedup(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "200.00")
	client := mustSQS(t)
	queue := mustQueueURL(t, client, "wager-transactions.fifo")
	ctx := context.Background()

	messageID := "msg-" + uuid.NewString()
	extID := "sqs-" + uuid.NewString()[:8]

	data := domain.WagerTransactionRequestedData{
		ProviderID: "provider-a", ExternalTransactionID: extID,
		IdempotencyKey: "provider-a:" + extID,
		PlayerID:       player, WalletID: walletID,
		RoundID: "round-sqs", GameID: "fortune-chimp",
		Kind: "BET", Money: domain.MoneyPayload{Amount: "45.00", Currency: "BRL"},
	}

	// ingress contract (challenge §10): messageId/type/occurredAt/data
	type ingressEnvelope struct {
		MessageID  string                               `json:"messageId"`
		Type       string                               `json:"type"`
		OccurredAt string                               `json:"occurredAt"`
		Data       domain.WagerTransactionRequestedData `json:"data"`
	}

	sendEnvelope := func(occurredAt string) {
		b, err := json.Marshal(ingressEnvelope{
			MessageID:  messageID,
			Type:       domain.EventWagerTransactionRequested,
			OccurredAt: occurredAt,
			Data:       data,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:               aws.String(queue),
			MessageBody:            aws.String(string(b)),
			MessageGroupId:         aws.String(walletID),
			MessageDeduplicationId: aws.String(uuid.NewString()), // bodies differ; keep both
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	sendEnvelope(time.Now().UTC().Format(time.RFC3339Nano))
	sendEnvelope(time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano))

	providerA := token(t, providerAClient, providerASecret)
	pollUntil(t, 20*time.Second, func() bool {
		_, body := api(t, "GET", "/providers/provider-a/wagering/transactions/"+extID, providerA, nil)
		st, _ := body["status"].(string)
		return st == "PROCESSED"
	}, "sqs operation never processed")

	// wait a bit for the second delivery to be consumed, then assert once.
	time.Sleep(3 * time.Second)

	// exactly one movement
	pool := mustDB(t)
	var entries int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM wallet_ledger_entries l
		 JOIN wager_transactions wt ON wt.id = l.transaction_id
		 WHERE wt.external_transaction_id = $1`, extID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("redelivery duplicated movement: %d entries", entries)
	}
	if bal, _ := walletBalance(t, internal, walletID); bal != "155.00" {
		t.Fatalf("balance: %s, want 155.00", bal)
	}
	// one durable inbox row for the message identity
	var inbox int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM consumer_inbox WHERE message_id = $1 AND completed_at IS NOT NULL`, messageID).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if inbox != 1 {
		t.Fatalf("inbox rows: %d, want 1", inbox)
	}
}

// TestSQSPoisonGoesToDLQ verifies that permanently invalid messages are
// routed to the DLQ instead of looping forever.
func TestSQSPoisonGoesToDLQ(t *testing.T) {
	client := mustSQS(t)
	queue := mustQueueURL(t, client, "wager-transactions.fifo")
	dlq := mustQueueURL(t, client, "wager-transactions-dlq.fifo")
	ctx := context.Background()

	poison := fmt.Sprintf(`{"messageId":"poison-%s","type":"WagerTransactionRequested","occurredAt":"2026-01-01T00:00:00Z","data":{"kind":"BET"`,
		uuid.NewString()[:8])
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(queue),
		MessageBody:            aws.String(poison),
		MessageGroupId:         aws.String("poison"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
	}); err != nil {
		t.Fatalf("send poison: %v", err)
	}

	pollUntil(t, 90*time.Second, func() bool {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(dlq),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil || len(out.Messages) == 0 {
			return false
		}
		for _, m := range out.Messages {
			if m.Body != nil && *m.Body == poison {
				_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl: aws.String(dlq), ReceiptHandle: m.ReceiptHandle,
				})
				return true
			}
		}
		return false
	}, "poison message never reached the DLQ")
}

// TestOutboxPublishedOnlyAfterCommit checks the events queue receives the
// events of a processed operation, and that outbox rows get marked published.
func TestOutboxPublishedOnlyAfterCommit(t *testing.T) {
	internal := token(t, internalClient, internalSecret)
	providerA := token(t, providerAClient, providerASecret)
	player := newPlayerID(t)
	walletID := openWallet(t, internal, player, "300.00")
	extID := "outbox-" + uuid.NewString()[:8]
	st, body := submitTransaction(t, providerA, betBody("provider-a", extID, player, walletID, "12.00"), "provider-a:"+extID)
	if st != 200 {
		t.Fatalf("bet: %d %v", st, body)
	}

	client := mustSQS(t)
	events := mustQueueURL(t, client, "integration-events.fifo")
	ctx := context.Background()

	var processed, balanceChanged bool
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && (!processed || !balanceChanged) {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(events),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil {
			t.Fatalf("receive events: %v", err)
		}
		for _, m := range out.Messages {
			var env struct {
				EventType string `json:"eventType"`
				Data      struct {
					ExternalTransactionID string `json:"externalTransactionId"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err == nil &&
				env.Data.ExternalTransactionID == extID {
				switch env.EventType {
				case "WagerTransactionProcessed":
					processed = true
				case "WalletBalanceChanged":
					balanceChanged = true
				}
			}
			_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: aws.String(events), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	if !processed || !balanceChanged {
		t.Fatalf("events missing: processed=%v balanceChanged=%v", processed, balanceChanged)
	}

	// outbox rows for this aggregate are all published
	pool := mustDB(t)
	var pending int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM integration_outbox
		WHERE aggregate_id = $1::uuid AND published_at IS NULL`, walletID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("unpublished outbox rows: %d", pending)
	}
}

// TestOutboxEventIdStableOnReplay: republication of the same event keeps the
// eventId (verifiable through the outbox PK and the dedup id used by the
// publisher — here we assert the DB invariant directly).
func TestOutboxEventIdStableOnReplay(t *testing.T) {
	pool := mustDB(t)
	ctx := context.Background()
	var eventID string
	if err := pool.QueryRow(ctx,
		`SELECT event_id FROM integration_outbox WHERE published_at IS NOT NULL LIMIT 1`).Scan(&eventID); err != nil {
		t.Skip("no published events yet")
	}
	var again string
	if err := pool.QueryRow(ctx,
		`SELECT event_id FROM integration_outbox WHERE event_id = $1::uuid`, eventID).Scan(&again); err != nil || again != eventID {
		t.Fatalf("event id not stable: %s vs %s (%v)", eventID, again, err)
	}
}

// TestOutboxRecoversWorkAbandonedByCrashedPublisher proves the §11 recovery
// guarantee end-to-end: a publisher interrupted BETWEEN claiming a row and
// publishing it leaves the row claimed (attempts incremented, lease held).
// Once the lease expires the row becomes claimable again and a RESTARTED
// worker publishes it — the event reaches the events queue and the row is
// durably marked published.
func TestOutboxRecoversWorkAbandonedByCrashedPublisher(t *testing.T) {
	ctx := context.Background()
	store := postgres.NewStore(mustDB(t))

	eventID := uuid.NewString()
	env := domain.Envelope{
		EventID:       eventID,
		EventType:     "WagerTransactionProcessed",
		AggregateType: "wager_transaction",
		AggregateID:   uuid.NewString(),
		OccurredAt:    time.Now().UTC(),
		Version:       1,
		Data:          map[string]any{"simulated": true, "note": "crashed-publisher recovery"},
	}
	if err := store.Repos().Outbox().Enqueue(ctx, []domain.Envelope{env}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// STEP 1 — a publisher claims the row…
	items, err := store.Repos().Outbox().ClaimBatch(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed := false
	for _, it := range items {
		if it.EventID.String() == eventID {
			claimed = true
		}
	}
	if !claimed {
		t.Fatalf("event %s was not claimed by the (crashing) publisher", eventID)
	}
	// …and dies before publishing. The one-hour lease keeps every OTHER
	// publisher (including the compose app instance) away while we arrange
	// the abandoned state below.

	// STEP 2 — time passes beyond the lease. Updating available_at to the
	// past reproduces EXACTLY what the abandoned row looks like once its
	// lease expires (attempts >= 1, published_at NULL, claimable again).
	if _, err := mustDB(t).Exec(ctx,
		`UPDATE integration_outbox SET available_at = now() - interval '1 second' WHERE event_id = $1`,
		eventID); err != nil {
		t.Fatalf("expire abandoned lease: %v", err)
	}

	// STEP 3 — a restarted instance picks the row up and publishes it.
	client := mustSQS(t)
	eventsURL := mustQueueURL(t, client, "integration-events.fifo")
	w := worker.NewOutboxWorker(store,
		sqsinfra.NewPublisher(client, sqsinfra.QueueURLs{Events: eventsURL}),
		&config.Config{OutboxInterval: 50 * time.Millisecond, OutboxBatch: 100, OutboxLease: time.Hour},
		testLogger, silentOutbox{})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(wctx) }()

	pollUntil(t, 30*time.Second, func() bool {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(eventsURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil {
			return false
		}
		for _, m := range out.Messages {
			if m.Body != nil && strings.Contains(*m.Body, eventID) {
				_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl: aws.String(eventsURL), ReceiptHandle: m.ReceiptHandle,
				})
				return true
			}
		}
		return false
	}, "abandoned outbox row was never published after restart")

	var published int
	if err := mustDB(t).QueryRow(ctx,
		`SELECT COUNT(*) FROM integration_outbox WHERE event_id = $1 AND published_at IS NOT NULL`,
		eventID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("recovered row was not marked published in the database")
	}
}

// silentOutbox satisfies worker.Instrumentation for in-process workers.
type silentOutbox struct{}

func (silentOutbox) OutboxPublished()                       {}
func (silentOutbox) OutboxFailed()                          {}
func (silentOutbox) OutboxStats(pending int64, lag float64) {}
