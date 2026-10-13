//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// The consumer is the one adapter that cannot be unit-tested: it depends on
// the concrete AWS SQS client and only makes sense against a real broker. It
// also runs inside the *app container* in the compose topology, so the
// coverage-instrumented test binary never touches it. This file closes that
// hole: it builds a real messaging.Consumer in-process, pointed at dedicated
// FIFO queues (so it never races the app's own consumer), and drives every
// terminal branch through the durability protocol.

// --- observation port -------------------------------------------------------

// recordingMetrics captures the consumer's observation callbacks so the test
// can assert on what the worker *reported*, not only on the DB side effects.
type recordingMetrics struct {
	mu         sync.Mutex
	received   int
	duplicates []string
	dlq        []string
	retries    int
	invalid    []string
}

func (m *recordingMetrics) Received() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received++
}
func (m *recordingMetrics) Duplicate(src string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.duplicates = append(m.duplicates, src)
}
func (m *recordingMetrics) DLQ(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dlq = append(m.dlq, reason)
}
func (m *recordingMetrics) Retry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries++
}
func (m *recordingMetrics) Invalid(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalid = append(m.invalid, reason)
}
func (m *recordingMetrics) StartSpan(ctx context.Context, _ string, _ ...messaging.SpanAttr) (context.Context, messaging.Span) {
	return ctx, noopSpan{}
}

func (m *recordingMetrics) dupCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.duplicates)
}
func (m *recordingMetrics) dlqCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.dlq)
}
func (m *recordingMetrics) retryCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.retries
}
func (m *recordingMetrics) receivedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.received
}

// noopSpan satisfies messaging.Span without pulling the tracing SDK in.
type noopSpan struct{}

func (noopSpan) SetAttributes(...messaging.SpanAttr) {}
func (noopSpan) SetOutcome(string, string)           {}
func (noopSpan) RecordError(error)                   {}
func (noopSpan) End()                                {}

var _ messaging.ConsumerMetrics = (*recordingMetrics)(nil)

// --- harness ----------------------------------------------------------------

// consumerHarness holds an isolated in-process consumer plus its own queues.
type consumerHarness struct {
	client *sqs.Client
	urls   sqsinfra.QueueURLs
	cfg    *config.Config
	store  *postgres.Store
	svc    *service.WagerService
	hook   *recordingMetrics
	cons   *messaging.Consumer
}

// uniqueQueue creates a FIFO queue (and its DLQ) with a test-scoped name so no
// run — or the app's own consumer — can steal messages from another.
func uniqueQueue(t *testing.T, client *sqs.Client, prefix string) (queueURL, dlqURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dlqName := fmt.Sprintf("%s-%s-dlq.fifo", prefix, uuid.NewString()[:8])
	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(dlqName),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "true",
		},
	})
	require.NoError(t, err, "create dlq")

	arnOut, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       dlq.QueueUrl,
		AttributeNames: []awssqstypes.QueueAttributeName{awssqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)

	name := fmt.Sprintf("%s-%s.fifo", prefix, uuid.NewString()[:8])
	q, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
			"RedrivePolicy": fmt.Sprintf(
				`{"deadLetterTargetArn":"%s","maxReceiveCount":"5"}`,
				arnOut.Attributes[string(awssqstypes.QueueAttributeNameQueueArn)]),
		},
	})
	require.NoError(t, err, "create queue")

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = client.DeleteQueue(cctx, &sqs.DeleteQueueInput{QueueUrl: q.QueueUrl})
		_, _ = client.DeleteQueue(cctx, &sqs.DeleteQueueInput{QueueUrl: dlq.QueueUrl})
	})
	return aws.ToString(q.QueueUrl), aws.ToString(dlq.QueueUrl)
}

// newConsumerHarness wires a fresh consumer against fresh queues and the real
// database. The config is real (env-derived) but with aggressive polling so
// tests finish fast.
func newConsumerHarness(t *testing.T) *consumerHarness {
	t.Helper()
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-consumer-test")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSMaxMessages = 10
	cfg.SQSVisibility = 2 // short: a redelivery is observable within the test
	cfg.ConsumerConcurrency = 2

	store := newTestStore(t)
	svc := newTestWagerService(t, store, 3, time.Minute)

	hook := &recordingMetrics{}
	urls := sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL}
	cons := messaging.NewConsumer(client, urls, cfg, store, svc, hook, testLogger)

	return &consumerHarness{
		client: client, urls: urls, cfg: cfg, store: store,
		svc: svc, hook: hook, cons: cons,
	}
}

// run starts the consumer loop and returns a stop function that waits for the
// loop to observe cancellation (so no goroutine leaks across subtests).
func (h *consumerHarness) run(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.cons.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer.Run did not return after cancel")
		}
	})
	return cancel
}

// send publishes one raw envelope onto the request queue.
func (h *consumerHarness) send(t *testing.T, messageID string, envelope any) {
	t.Helper()
	b, err := json.Marshal(envelope)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = h.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(h.urls.Request),
		MessageBody:            aws.String(string(b)),
		MessageGroupId:         aws.String("test-group"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	require.NoError(t, err, "send")
}

// sendRaw publishes an arbitrary body (for malformed / unknown-type cases).
func (h *consumerHarness) sendRaw(t *testing.T, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(h.urls.Request),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String("test-group"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	require.NoError(t, err, "send raw")
}

// envelope is the ingress contract shape (messageId/type/occurredAt/data).
func envelope(messageID, kind string, data any) map[string]any {
	return map[string]any{
		"messageId":  messageID,
		"type":       domain.EventWagerTransactionRequested,
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data":       data,
	}
}

func wagerData(provider, extID, playerID, walletID, kind, amount string) domain.WagerTransactionRequestedData {
	return domain.WagerTransactionRequestedData{
		ProviderID:            provider,
		ExternalTransactionID: extID,
		IdempotencyKey:        "idem-" + extID,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-" + extID,
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 domain.MoneyPayload{Amount: amount, Currency: "BRL"},
	}
}

// drainDLQ waits for a DLQ message to appear.
func (h *consumerHarness) drainDLQ(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := h.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(h.urls.DLQ),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		cancel()
		if err == nil && len(out.Messages) > 0 {
			return aws.ToString(out.Messages[0].Body)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ""
}

// --- failing transaction manager --------------------------------------------

// failTx is a service.TxManager that always fails with a generic error. It
// lets the test drive the consumer's *transient* branch deterministically: a
// non-terminal, non-poison failure that must be retried (and, once the
// receive count is exhausted, moved to the DLQ) rather than acked.
type failTx struct {
	repos service.Repos
}

func (f *failTx) WithinTx(context.Context, func(context.Context, service.Repos) error) error {
	return errors.New("injected transient infrastructure failure")
}
func (f *failTx) Repos() service.Repos { return f.repos }

var _ service.TxManager = (*failTx)(nil)

// --- tests ------------------------------------------------------------------

func TestConsumerProcessesValidMessageInProcess(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	extID := "ext-" + uuid.NewString()[:8]
	msgID := uuid.NewString()

	h.send(t, msgID, envelope(msgID, "BET", wagerData("provider-a", extID, player, walletID, "BET", "25.00")))

	// The transaction becomes visible in the database as PROCESSED.
	require.Eventually(t, func() bool {
		var status string
		err := mustDB(t).QueryRow(context.Background(),
			`SELECT status FROM wager_transactions WHERE external_transaction_id=$1`, extID).Scan(&status)
		return err == nil && status == string(domain.StatusProcessed)
	}, 20*time.Second, 250*time.Millisecond, "BET never processed by in-process consumer")

	assert.GreaterOrEqual(t, h.hook.receivedCount(), 1, "consumer must report the receipt")
}

func TestConsumerRedeliveryIsDeduplicatedByInbox(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	extID := "ext-" + uuid.NewString()[:8]
	// Same logical message delivered twice → same messageId, same body. The id
	// must be unique per RUN: the inbox primary key is (consumer_name,
	// message_id) and the table is append-only, so a reused literal would
	// collide with a previous run's row.
	msgID := "msg-dup-" + uuid.NewString()
	env := envelope(msgID, "BET", wagerData("provider-a", extID, player, walletID, "BET", "25.00"))

	h.send(t, msgID, env)
	require.Eventually(t, func() bool {
		var n int
		_ = mustDB(t).QueryRow(context.Background(),
			`SELECT COUNT(*) FROM wager_transactions WHERE external_transaction_id=$1`, extID).Scan(&n)
		return n == 1
	}, 20*time.Second, 250*time.Millisecond)

	h.send(t, msgID, env)       // redelivery
	time.Sleep(4 * time.Second) // let it be consumed

	// Exactly one movement: the inbox replay must not move money twice.
	var entries int
	require.NoError(t, mustDB(t).QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wallet_ledger_entries l
		 JOIN wager_transactions wt ON wt.id = l.transaction_id
		 WHERE wt.external_transaction_id=$1`, extID).Scan(&entries))
	assert.Equal(t, 1, entries, "redelivery must not create a second ledger movement")

	bal := walletBalanceDirect(t, walletID)
	assert.Equal(t, "75.00", bal, "balance must reflect exactly one debit")
}

func TestConsumerUnknownEventTypeIsPoisonedToDLQ(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	body := `{"messageId":"` + uuid.NewString() + `","type":"SomethingElse","occurredAt":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `","data":{}}`
	h.sendRaw(t, body)

	got := h.drainDLQ(t, 20*time.Second)
	require.NotEmpty(t, got, "unknown event type must land in the DLQ")
	assert.Contains(t, got, "SomethingElse")
	assert.Eventually(t, func() bool { return h.hook.dlqCount() >= 1 },
		5*time.Second, 100*time.Millisecond, "DLQ must be reported to the observation port")
}

func TestConsumerMalformedEnvelopeIsPoisonedToDLQ(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	// Valid JSON, unknown type field missing → decoded but type mismatch.
	h.sendRaw(t, `{"messageId":"`+uuid.NewString()+`","occurredAt":"`+
		time.Now().UTC().Format(time.RFC3339Nano)+`","data":{"kind":"BET"}}`)

	require.NotEmpty(t, h.drainDLQ(t, 20*time.Second), "missing/blank event type must reach the DLQ")
}

func TestConsumerPoisonDataIsDLQ(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	// Correct event type, but data is not an object → unparseable data branch.
	body := `{"messageId":"` + uuid.NewString() + `","type":"` + domain.EventWagerTransactionRequested +
		`","occurredAt":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","data":"not-an-object"}`
	h.sendRaw(t, body)

	require.NotEmpty(t, h.drainDLQ(t, 20*time.Second), "unparseable data must reach the DLQ")
}

func TestConsumerTerminalBusinessErrorIsDLQ(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	// BET against an unknown wallet → NOT FOUND is terminal: no redelivery can
	// ever succeed, so the message must be DLQ'd rather than spin.
	extID := "ext-" + uuid.NewString()[:8]
	msgID := uuid.NewString()
	h.send(t, msgID, envelope(msgID, "BET",
		wagerData("provider-a", extID, uuid.NewString(), uuid.NewString(), "BET", "10.00")))

	require.NotEmpty(t, h.drainDLQ(t, 20*time.Second), "terminal business error must reach the DLQ")
}

func TestConsumerBusinessShapeInvalidIsDLQ(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	// A negative amount fails domain validation in Prepare → poison, before any
	// inbox row is even attempted.
	extID := "ext-" + uuid.NewString()[:8]
	msgID := uuid.NewString()
	h.send(t, msgID, envelope(msgID, "BET",
		wagerData("provider-a", extID, uuid.NewString(), uuid.NewString(), "BET", "-5.00")))

	require.NotEmpty(t, h.drainDLQ(t, 20*time.Second), "invalid business shape must reach the DLQ")
}

// TestConsumerHashMismatchIsPoison pins the anomalous case where the SAME
// messageId is reused with DIFFERENT content: the inbox already holds the
// message id, so it cannot be reprocessed, and the divergence must be a
// poison → DLQ, never a silent ack.
func TestConsumerHashMismatchIsPoison(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	msgID := "msg-same-" + uuid.NewString()[:8]

	first := envelope(msgID, "BET", wagerData("provider-a", "ext-a-"+uuid.NewString()[:8], player, walletID, "BET", "10.00"))
	h.send(t, msgID, first)

	// Wait until the first is durably handled (inbox row committed).
	require.Eventually(t, func() bool {
		var n int
		_ = mustDB(t).QueryRow(context.Background(),
			`SELECT COUNT(*) FROM consumer_inbox WHERE message_id=$1`, msgID).Scan(&n)
		return n == 1
	}, 20*time.Second, 250*time.Millisecond, "first delivery never recorded in the inbox")

	// Reuse the SAME messageId with DIFFERENT content.
	second := envelope(msgID, "BET", wagerData("provider-a", "ext-b-"+uuid.NewString()[:8], player, walletID, "BET", "99.00"))
	// Force a distinct body under the same messageId (dedup id differs anyway).
	h.sendRaw(t, mustJSON(t, second))

	require.NotEmpty(t, h.drainDLQ(t, 20*time.Second), "messageId reuse with different content must be poisoned")
	assert.Eventually(t, func() bool { return h.hook.dupCount() >= 1 },
		10*time.Second, 100*time.Millisecond, "the duplicate must be reported before poisoning")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// slowTx fails transiently but first blocks long enough that an in-flight
// handler outlives a deliberately tiny shutdown grace.
type slowTx struct{}

func (slowTx) WithinTx(context.Context, func(context.Context, service.Repos) error) error {
	time.Sleep(3 * time.Second)
	return errors.New("slow transient failure")
}
func (slowTx) Repos() service.Repos { return nil }

var _ service.TxManager = slowTx{}

// TestConsumerShutdownGraceElapsed covers the drain branch where in-flight work
// does not finish within ShutdownGrace: the loop must log the warning and still
// return nil (messages lose visibility and are safely redelivered).
func TestConsumerShutdownGraceElapsed(t *testing.T) {
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-grace-test")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.ShutdownGrace = 50 * time.Millisecond // far shorter than slowTx's sleep

	svc := service.NewWagerService(slowTx{}, testLogger, &silentInstrumentation{})
	hook := &recordingMetrics{}
	urls := sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL}
	cons := messaging.NewConsumer(client, urls, cfg, slowTx{}, svc, hook, testLogger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cons.Run(ctx) }()

	msgID := uuid.NewString()
	body := mustJSON(t, envelope(msgID, "BET",
		wagerData("provider-a", "ext-"+uuid.NewString()[:8], uuid.NewString(), uuid.NewString(), "BET", "10.00")))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_, err = client.SendMessage(ctx2, &sqs.SendMessageInput{
		QueueUrl: aws.String(rootURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	require.NoError(t, err)

	// Let the handler pick the message up and enter its long sleep.
	require.Eventually(t, func() bool { return hook.receivedCount() >= 1 },
		15*time.Second, 100*time.Millisecond, "handler never started")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "Run returns nil even when the grace elapses")
	case <-time.After(20 * time.Second):
		t.Fatal("consumer.Run did not return")
	}
}

// that is neither poison nor a terminal business error must be released for
// redelivery (Retry reported, message NOT acked, NOT DLQ'd).
func TestConsumerTransientFailureIsRetried(t *testing.T) {
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-transient-test")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSMaxReceive = 5 // plenty of runway: this delivery must be retried, not DLQ'd

	store := newTestStore(t)
	_ = store
	// Both the use case and the consumer's transaction manager fail
	// transiently: WithinTx errors before the callback runs, so the consumer
	// must classify it as retryable (not a terminal business error).
	failing := &failTx{}
	svc := service.NewWagerService(failing, testLogger, &silentInstrumentation{})
	hook := &recordingMetrics{}
	urls := sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL}
	cons := messaging.NewConsumer(client, urls, cfg, failing, svc, hook, testLogger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cons.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer.Run did not return after cancel")
		}
	}()

	msgID := uuid.NewString()
	body := mustJSON(t, envelope(msgID, "BET",
		wagerData("provider-a", "ext-"+uuid.NewString()[:8], uuid.NewString(), uuid.NewString(), "BET", "10.00")))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_, err = client.SendMessage(ctx2, &sqs.SendMessageInput{
		QueueUrl: aws.String(rootURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return hook.retryCount() >= 1 },
		20*time.Second, 200*time.Millisecond, "a transient failure must be retried")
	assert.Zero(t, hook.dlqCount(), "a retryable failure must not be DLQ'd while attempts remain")
}

// TestConsumerRetriesExhaustedIsDLQ covers the transient branch's terminal
// case: once the SQS receive count reaches the configured maximum, the message
// stops being retried and is moved to the DLQ.
func TestConsumerRetriesExhaustedIsDLQ(t *testing.T) {
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-exhaust-test")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSMaxReceive = 1 // the very first delivery is already "exhausted"

	store := newTestStore(t)
	_ = store
	failing := &failTx{}
	svc := service.NewWagerService(failing, testLogger, &silentInstrumentation{})
	hook := &recordingMetrics{}
	urls := sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL, Events: rootURL}
	cons := messaging.NewConsumer(client, urls, cfg, failing, svc, hook, testLogger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cons.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer.Run did not return after cancel")
		}
	}()

	msgID := uuid.NewString()
	body := mustJSON(t, envelope(msgID, "BET",
		wagerData("provider-a", "ext-"+uuid.NewString()[:8], uuid.NewString(), uuid.NewString(), "BET", "10.00")))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_, err = client.SendMessage(ctx2, &sqs.SendMessageInput{
		QueueUrl: aws.String(rootURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String("g"), MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return hook.dlqCount() >= 1 },
		25*time.Second, 250*time.Millisecond, "exhausted retries must land in the DLQ")
	assert.Zero(t, hook.retryCount(), "with maxReceive=1 the message must not be counted as a retry")
}

// TestConsumerUnparseableEnvelopeIsPoisoned covers the branch where the raw
// body is not even valid JSON: it can never be processed, so it must be moved
// to the DLQ without ever opening a transaction.
func TestConsumerUnparseableEnvelopeIsPoisoned(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	h.sendRaw(t, "this is not json at all {")

	got := h.drainDLQ(t, 20*time.Second)
	require.NotEmpty(t, got, "unparseable envelope must reach the DLQ")
	assert.Contains(t, got, "not json")
}

// TestConsumerIncompleteInboxRowIsAcknowledged pins the anomalous
// "inbox row present without completion" branch: an inbox row whose hash
// matches but that carries no completed_at is treated as already handled (the
// committed row wins) and the delivery is acked rather than reprocessed.
//
// Reaching it deterministically means planting the row ourselves: the message
// id and the sha256 of the raw body are the inbox identity the consumer looks
// up.
func TestConsumerIncompleteInboxRowIsAcknowledged(t *testing.T) {
	h := newConsumerHarness(t)
	h.run(t)

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	extID := "ext-" + uuid.NewString()[:8]
	msgID := "incomplete-" + uuid.NewString()[:8]

	body := mustJSON(t, envelope(msgID, "BET",
		wagerData("provider-a", extID, player, walletID, "BET", "10.00")))

	// Plant an inbox row with the SAME hash the consumer will compute, but
	// leave completed_at NULL.
	sum := sha256.Sum256([]byte(body))
	_, err := mustDB(t).Exec(context.Background(),
		`INSERT INTO consumer_inbox (consumer_name, message_id, message_hash, completed_at)
		 VALUES ($1, $2, $3, NULL)`,
		"sqs-wager-consumer", msgID, hex.EncodeToString(sum[:]))
	require.NoError(t, err, "plant incomplete inbox row")

	h.sendRaw(t, body)

	// The message is acked (removed) and NOT processed: no transaction is
	// created, and nothing lands in the DLQ.
	time.Sleep(4 * time.Second)
	var n int
	require.NoError(t, mustDB(t).QueryRow(context.Background(),
		`SELECT COUNT(*) FROM wager_transactions WHERE external_transaction_id=$1`, extID).Scan(&n))
	assert.Equal(t, 0, n, "an incomplete-but-matching inbox row must not be reprocessed")

	// The queue must be empty: if the message had been retried it would still
	// be visible after the short visibility timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := h.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(h.urls.Request),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     2,
	})
	require.NoError(t, err)
	if len(out.Messages) > 0 {
		t.Fatalf("message was not acked; still visible: %v", out.Messages)
	}

	assert.GreaterOrEqual(t, h.hook.dupCount(), 1, "the matching replay must be reported")
	assert.Zero(t, h.hook.retryCount(), "an already-handled message is not a retry")
}

// TestConsumerReceiveFailureBacksOffThenStops covers the intake error branch:
// when ReceiveMessage fails (here: a queue URL that does not exist) the loop
// must log, back off, and keep going until the context is cancelled, then
// return cleanly.
func TestConsumerReceiveFailureBacksOffThenStops(t *testing.T) {
	client := mustSQS(t)
	store := newTestStore(t)
	svc := newTestWagerService(t, store, 3, time.Minute)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.ConsumerConcurrency = 1

	urls := sqsinfra.QueueURLs{
		Request: "http://localhost:4566/000000000000/does-not-exist-" + uuid.NewString()[:8] + ".fifo",
		DLQ:     "http://localhost:4566/000000000000/nope-dlq.fifo",
	}
	hook := &recordingMetrics{}
	cons := messaging.NewConsumer(client, urls, cfg, store, svc, hook, testLogger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cons.Run(ctx) }()

	// Give the loop time to fail at least one ReceiveMessage and enter its
	// one-second back-off select.
	time.Sleep(2500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "Run must return nil on context cancellation")
	case <-time.After(15 * time.Second):
		t.Fatal("consumer.Run did not return after cancel")
	}
	assert.Zero(t, hook.receivedCount(), "no message can be received from a missing queue")
}

// --- small assertions helpers ----------------------------------------------

func openWalletDirect(t *testing.T, player, amount string) string {
	t.Helper()
	ws := service.NewWalletService(newTestStore(t), testLogger, &silentInstrumentation{})
	view, err := ws.Open(context.Background(), service.OpenWalletInput{
		PlayerID: player, InitialAmount: amount, Currency: "BRL",
		IdempotencyKey: "open-" + uuid.NewString(),
	})
	require.NoError(t, err)
	return view.ID.String()
}

func walletBalanceDirect(t *testing.T, walletID string) string {
	t.Helper()
	ws := service.NewWalletService(newTestStore(t), testLogger, &silentInstrumentation{})
	view, err := ws.Get(context.Background(), walletID)
	require.NoError(t, err)
	return view.Balance.String()
}

// Ensure the helpers stay wired to the real service.
var _ = errors.Is
