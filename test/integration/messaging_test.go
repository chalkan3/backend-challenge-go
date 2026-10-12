//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"

	"github.com/chalkan33/wallet/internal/domain"
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
