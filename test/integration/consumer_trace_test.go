//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	platformotel "github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// Consumer-side trace continuity.
//
// TestTraceContextSurvivesOutboxAndSQS proves the trace survives the *wire*
// (row → claim → SQS attribute). This test closes the loop on the *consumer*:
// the running Consumer must actually READ that attribute and resume the trace
// BEFORE opening the handle span, so wallet.sqs.consume becomes a child of the
// originating request instead of the root of an orphan trace.
//
// It drives the real Consumer.Run against a real queue and the real database,
// and inspects the context the consumer handed to StartSpan — i.e. the exact
// context the handle span would be opened in.
func TestConsumerResumesTraceFromMessageAttribute(t *testing.T) {
	const (
		sampleTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		sampleSpanID  = "00f067aa0ba902b7"
	)
	carrier := platformotel.NewCarrier()

	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-trace-consumer")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSMaxMessages = 10
	cfg.SQSVisibility = 2
	cfg.ConsumerConcurrency = 1

	store := postgres.NewStore(mustDB(t), carrier)
	svc := service.NewWagerService(store, testLogger, &silentInstrumentation{})
	hook := &recordingMetrics{}

	// The consumer is wired with a REAL carrier — the thing under test. With a
	// nil extractor the resume would be skipped and this test would fail.
	cons := newConsumerWithCarrier(t, client, sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL},
		cfg, store, svc, hook, carrier)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cons.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer.Run did not return after cancel")
		}
	})

	// A wallet must exist so the wager can be applied (the consumer acts as the
	// internal actor; a BET on a funded wallet exercises the happy path).
	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")

	// Publish a real request carrying the sample traceparent as a message
	// attribute — exactly the shape sqs.Publisher emits.
	extID := "ext-trace-" + uuid.NewString()[:8]
	env := envelope("msg-trace-"+uuid.NewString()[:8], "BET",
		wagerData("provider-a", extID, player, walletID, "BET", "10.00"))
	body, err := json.Marshal(env)
	require.NoError(t, err)

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sendCancel()
	_, err = client.SendMessage(sendCtx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(rootURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String("trace-group"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
		MessageAttributes: map[string]awssqstypes.MessageAttributeValue{
			sqsinfra.TraceParentAttribute: {
				DataType:    aws.String("String"),
				StringValue: aws.String("00-" + sampleTraceID + "-" + sampleSpanID + "-01"),
			},
		},
	})
	require.NoError(t, err, "send traced request")
	// The consumer must process the message AND hand StartSpan a context that
	// already carries the producer's trace as its remote parent.
	require.Eventually(t, func() bool {
		hook.mu.Lock()
		defer hook.mu.Unlock()
		return hook.handleSpanCtx != nil && hook.received > 0
	}, 20*time.Second, 100*time.Millisecond, "consumer never handled the message")

	hook.mu.Lock()
	spanCtx := hook.handleSpanCtx
	hook.mu.Unlock()

	sc := trace.SpanContextFromContext(spanCtx)
	require.True(t, sc.IsValid(), "handle context must carry the resumed remote span")
	assert.Equal(t, sampleTraceID, sc.TraceID().String(),
		"consumer handle must run in the producer's trace")
	assert.Equal(t, sampleSpanID, sc.SpanID().String(),
		"consumer handle must name the producer span as its parent")
}

// TestConsumerStartsFreshTraceWithoutAttribute is the negative control: a
// message with NO traceparent must not fabricate one. The handle span then
// starts a new trace, which is the correct behaviour for a producer that had
// no upstream context.
func TestConsumerStartsFreshTraceWithoutAttribute(t *testing.T) {
	client := mustSQS(t)
	rootURL, dlqURL := uniqueQueue(t, client, "wager-notrace-consumer")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.SQSWaitSeconds = 1
	cfg.SQSVisibility = 2
	cfg.ConsumerConcurrency = 1

	store := postgres.NewStore(mustDB(t), platformotel.NewCarrier())
	svc := service.NewWagerService(store, testLogger, &silentInstrumentation{})
	hook := &recordingMetrics{}
	cons := newConsumerWithCarrier(t, client, sqsinfra.QueueURLs{Request: rootURL, DLQ: dlqURL},
		cfg, store, svc, hook, platformotel.NewCarrier())

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cons.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumer.Run did not return after cancel")
		}
	})

	player := uuid.NewString()
	walletID := openWalletDirect(t, player, "100.00")
	extID := "ext-notrace-" + uuid.NewString()[:8]
	env := envelope("msg-notrace-"+uuid.NewString()[:8], "BET",
		wagerData("provider-a", extID, player, walletID, "BET", "10.00"))
	body, err := json.Marshal(env)
	require.NoError(t, err)

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sendCancel()
	_, err = client.SendMessage(sendCtx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(rootURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String("notrace-group"),
		MessageDeduplicationId: aws.String(uuid.NewString()),
		// deliberately no MessageAttributes
	})
	require.NoError(t, err, "send untraced request")

	require.Eventually(t, func() bool {
		hook.mu.Lock()
		defer hook.mu.Unlock()
		return hook.handleSpanCtx != nil
	}, 20*time.Second, 100*time.Millisecond, "consumer never handled the message")

	hook.mu.Lock()
	spanCtx := hook.handleSpanCtx
	hook.mu.Unlock()

	assert.False(t, trace.SpanContextFromContext(spanCtx).IsValid(),
		"no attribute must yield an empty (fresh-trace) context, not a fabricated one")
}

// newConsumerWithCarrier wires a Consumer exactly like newConsumerHarness, but
// with a real trace extractor instead of nil — the difference under test.
func newConsumerWithCarrier(
	t *testing.T, client *sqs.Client, urls sqsinfra.QueueURLs, cfg *config.Config,
	store *postgres.Store, svc *service.WagerService, hook *recordingMetrics,
	extractor messaging.TraceExtractor,
) *messaging.Consumer {
	t.Helper()
	return messaging.NewConsumer(client, urls, cfg, store, svc, hook, extractor, testLogger)
}
