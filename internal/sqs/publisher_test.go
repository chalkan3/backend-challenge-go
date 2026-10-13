// Unit tests for the outbox publisher, the last hop before SQS.
//
// The routing fields are the contract: MessageGroupId carries the aggregate id
// so one wallet's events stay ordered, and MessageDeduplicationId carries the
// event id so SQS itself drops the duplicates that at-least-once outbox
// republication produces. Get either wrong and the guarantee breaks silently —
// ordering lost, or the same event delivered twice — which is exactly why they
// are pinned here against a broker stub that echoes what it received.
package sqs_test

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/service"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// sentMessage is what the broker stub captured from one SendMessage call.
type sentMessage struct {
	QueueURL               string `json:"QueueUrl"`
	MessageBody            string `json:"MessageBody"`
	MessageGroupId         string `json:"MessageGroupId"`
	MessageDeduplicationId string `json:"MessageDeduplicationId"`
	MessageAttributes      map[string]struct {
		DataType    string `json:"DataType"`
		StringValue string `json:"StringValue"`
	} `json:"MessageAttributes"`
}

func newPublisherStub(t *testing.T) (*sqs.Publisher, *[]sentMessage, *httptest.Server) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []sentMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload sentMessage
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		seen = append(seen, payload)
		mu.Unlock()
		// The SDK validates the body checksum it asked us to echo, so the stub
		// computes the real MD5 rather than a placeholder.
		sum := md5.Sum([]byte(payload.MessageBody))
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		fmt.Fprintf(w, `{"MessageId":"m","MD5OfMessageBody":"%x"}`, sum)
	}))
	t.Cleanup(srv.Close)

	client := awssqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", ""),
	}, func(o *awssqs.Options) { o.BaseEndpoint = aws.String(srv.URL) })

	urls := sqs.QueueURLs{Events: srv.URL + "/queue/events"}
	return sqs.NewPublisher(client, urls), &seen, srv
}

// TestPublisherRoutesByAggregateAndDeduplicatesByEvent pins the two routing
// fields and the target queue in a single assertion set: the body travels
// verbatim, ordering is per aggregate and deduplication is per event.
func TestPublisherRoutesByAggregateAndDeduplicatesByEvent(t *testing.T) {
	pub, seen, srv := newPublisherStub(t)
	eventID := uuid.New()

	item := service.OutboxItem{
		EventID:     eventID,
		AggregateID: "wallet-42",
		EventType:   "WagerTransactionProcessed",
		Payload:     []byte(`{"hello":"world"}`),
	}
	require.NoError(t, pub.Publish(context.Background(), item))

	msgs := *seen
	require.Len(t, msgs, 1)
	m := msgs[0]
	assert.Equal(t, srv.URL+"/queue/events", m.QueueURL)
	assert.Equal(t, `{"hello":"world"}`, m.MessageBody, "the payload travels verbatim")
	assert.Equal(t, "wallet-42", m.MessageGroupId, "ordering is per aggregate")
	assert.Equal(t, eventID.String(), m.MessageDeduplicationId, "dedup is per event")
	assert.Empty(t, m.MessageAttributes, "no traceparent means no attribute at all")
}

// TestPublisherForwardsTraceParent covers the trace bridge: when the enqueue
// captured a traceparent, the publisher must attach it as a custom attribute
// so the consumer resumes the originating trace. This is the difference
// between one trace spanning HTTP → outbox → SQS → consumer, and two.
func TestPublisherForwardsTraceParent(t *testing.T) {
	pub, seen, _ := newPublisherStub(t)
	traceParent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	item := service.OutboxItem{
		EventID:     uuid.New(),
		AggregateID: "wallet-7",
		Payload:     []byte(`{}`),
		TraceParent: traceParent,
	}
	require.NoError(t, pub.Publish(context.Background(), item))

	msgs := *seen
	require.Len(t, msgs, 1)
	attr, ok := msgs[0].MessageAttributes[sqs.TraceParentAttribute]
	require.True(t, ok, "the traceparent must be attached as %q", sqs.TraceParentAttribute)
	assert.Equal(t, "String", attr.DataType)
	assert.Equal(t, traceParent, attr.StringValue)
}

// TestPublisherReturnsSendError pins that a broker failure is surfaced, not
// swallowed: the relay worker relies on the error to schedule a retry, and an
// event that vanished without an error would never be redelivered.
func TestPublisherReturnsSendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"__type":"InternalError","message":"boom"}`))
	}))
	defer srv.Close()

	client := awssqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", ""),
	}, func(o *awssqs.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	pub := sqs.NewPublisher(client, sqs.QueueURLs{Events: srv.URL + "/queue/events"})

	err := pub.Publish(context.Background(), service.OutboxItem{
		EventID: uuid.New(), AggregateID: "w", Payload: []byte(`{}`),
	})
	require.Error(t, err)
}
