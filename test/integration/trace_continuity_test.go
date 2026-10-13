//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	platformotel "github.com/chalkan3/backend-challenge-go/internal/platform/otel"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// Cross-process trace continuity.
//
// The outbox breaks a trace in two by construction: the HTTP request commits a
// row and returns, while a DETACHED relay publishes it afterwards from a
// context born of context.Background(). Nothing in that second context points
// back to the request, so the provider would otherwise see two unrelated
// traces (HTTP submit | relay → SQS → consumer).
//
// This test proves the bridge is real end to end — every hop of the production
// path, against PostgreSQL and LocalStack, no mocks:
//
//	HTTP span → Enqueue (trace_parent column) → ClaimBatch (read back)
//	          → sqs.Publisher (message attribute) → ReceiveMessage
//	          → Extract (consumer parent)
//
// The failure it is built to catch: a refactor that stops persisting the
// traceparent, or drops the SQS attribute, leaving the consumer trace
// orphaned. In that case the final assertion fails with two different trace
// IDs.
func TestTraceContextSurvivesOutboxAndSQS(t *testing.T) {
	ctx := context.Background()

	// The producer is a real tracer provider, not a stub: propagation is only
	// meaningful for spans the SDK actually produced.
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(ctx) }()
	carrier := platformotel.NewCarrier()

	// A sample traceparent to pin the expectation (W3C sample value).
	const sampleTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const sampleSpanID = "00f067aa0ba902b7"
	const sampleParent = "00-" + sampleTraceID + "-" + sampleSpanID + "-01"

	// The store is wired with the real carrier — exactly as bootstrap wires it
	// in production — so Enqueue captures the active trace context.
	store := postgres.NewStore(mustDB(t), carrier)

	// STEP 1 — the "HTTP request": a span carrying the sample trace, then the
	// use case enqueues inside it. Inject what otelhttp hands the handler.
	httpCtx := carrier.Extract(ctx, sampleParent)
	httpCtx, httpSpan := tp.Tracer("test").Start(httpCtx, "wallet.http.submit_transaction")
	defer httpSpan.End()

	eventID := uuid.NewString()
	env := domain.Envelope{
		EventID:       eventID,
		EventType:     "WagerTransactionProcessed",
		AggregateType: "wager_transaction",
		AggregateID:   uuid.NewString(),
		OccurredAt:    time.Now().UTC(),
		Version:       1,
		Data:          map[string]any{"simulated": true, "note": "trace continuity"},
	}
	if err := store.Repos().Outbox().Enqueue(httpCtx, []domain.Envelope{env}); err != nil {
		t.Fatalf("enqueue inside HTTP span: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mustDB(t).Exec(context.Background(),
			`DELETE FROM integration_outbox WHERE event_id = $1`, eventID)
	})

	// STEP 2 — the row must carry the HTTP trace, not an empty string. This is
	// the half of the bridge the schema was extended for.
	var storedTrace string
	if err := mustDB(t).QueryRow(ctx,
		`SELECT trace_parent FROM integration_outbox WHERE event_id = $1`, eventID).
		Scan(&storedTrace); err != nil {
		t.Fatalf("read trace_parent: %v", err)
	}
	if !strings.Contains(storedTrace, sampleTraceID) {
		t.Fatalf("outbox row lost the HTTP trace: trace_parent=%q, want trace %s", storedTrace, sampleTraceID)
	}

	// STEP 3 — the relay claims the row: the traceparent must come back as
	// data, since the relay's own context is detached from the request.
	items, err := store.Repos().Outbox().ClaimBatch(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var claimedTrace string
	var claimedItem service.OutboxItem
	var claimedFound bool
	for _, it := range items {
		if it.EventID.String() == eventID {
			claimedItem, claimedTrace, claimedFound = it, it.TraceParent, true
		}
	}
	if !claimedFound {
		t.Fatalf("event %s was not claimed", eventID)
	}
	if !strings.Contains(claimedTrace, sampleTraceID) {
		t.Fatalf("ClaimBatch dropped the traceparent: got %q", claimedTrace)
	}

	// STEP 4 — publish for real. The publisher forwards the traceparent as a
	// SQS message attribute so it survives the queue.
	client := mustSQS(t)
	eventsURL := mustQueueURL(t, client, "integration-events.fifo")
	pub := sqsinfra.NewPublisher(client, sqsinfra.QueueURLs{Events: eventsURL})
	if err := pub.Publish(ctx, claimedItem); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// STEP 5 — receive it back and confirm the attribute made the trip.
	msg := receiveMatching(t, client, eventsURL, eventID)
	got, ok := msg.MessageAttributes[sqsinfra.TraceParentAttribute]
	if !ok || got.StringValue == nil || !strings.Contains(*got.StringValue, sampleTraceID) {
		t.Fatalf("SQS message lost the traceparent attribute: %#v", msg.MessageAttributes)
	}

	// STEP 6 — the consumer side: extracting the attribute must land the
	// consumer's span in the SAME trace as the original HTTP request, parented
	// by the exact span that enqueued the event (the active HTTP span), not by
	// the network-level sample that preceded it.
	consumerCtx := carrier.Extract(context.Background(), *got.StringValue)
	_, consumerSpan := tp.Tracer("consumer").Start(consumerCtx, "wallet.sqs.consume")
	defer consumerSpan.End()

	if consumerSpan.SpanContext().TraceID().String() != sampleTraceID {
		t.Fatalf("trace broke at the consumer: got trace %s, want %s",
			consumerSpan.SpanContext().TraceID(), sampleTraceID)
	}
	wantParent := httpSpan.SpanContext().SpanID()
	gotParent := trace.SpanContextFromContext(consumerCtx)
	if gotParent.SpanID() != wantParent {
		t.Fatalf("consumer parent is not the enqueuing HTTP span: got %s, want %s",
			gotParent.SpanID(), wantParent)
	}
	// The whole point: the consumer is a descendant of the request that moved
	// the money, not the root of an orphan trace.
	if consumerSpan.SpanContext().TraceID() != httpSpan.SpanContext().TraceID() {
		t.Fatalf("consumer trace %s is not the HTTP trace %s",
			consumerSpan.SpanContext().TraceID(), httpSpan.SpanContext().TraceID())
	}
}

// receiveMatching polls the queue until the message carrying eventID arrives,
// deleting it on the way out so the queue stays clean for other tests. The
// events queue is FIFO and shared with the compose workers, so it must match
// on the body rather than assume the message is first.
func receiveMatching(t *testing.T, client *sqs.Client, queueURL, eventID string) awssqstypes.Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       2,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Logf("receive: %v", err)
			continue
		}
		for _, m := range out.Messages {
			if m.Body == nil || !strings.Contains(*m.Body, eventID) {
				continue
			}
			_, _ = client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: aws.String(queueURL), ReceiptHandle: m.ReceiptHandle,
			})
			return m
		}
	}
	t.Fatalf("event %s never arrived on the events queue", eventID)
	return awssqstypes.Message{}
}
