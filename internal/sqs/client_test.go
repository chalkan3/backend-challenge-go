// Unit tests for the SQS client's startup path. The happy path (resolving all
// three queue URLs) is covered end to end against LocalStack; here we pin the
// fail-fast branch: a broker the client cannot reach must stop startup, not
// hand back an empty URL that would make the first SendMessage fail much later,
// far from its cause.
package sqs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

func unreachableConfig() *config.Config {
	return &config.Config{
		SQSEndpoint:     "http://127.0.0.1:1", // reserved port, never listening
		SQSRegion:       "us-east-1",
		SQSAccessKeyID:  "test",
		SQSSecretKey:    "test",
		SQSRequestQueue: "wager-requests.fifo",
		SQSEventsQueue:  "integration-events.fifo",
		SQSDLQQueue:     "wager-dlq.fifo",
	}
}

// TestResolveQueueURLsFailsFastOnUnreachableBroker pins the contract: an
// unreachable broker makes ResolveQueueURLs return an error naming the queue,
// instead of a zero-valued URL set.
func TestResolveQueueURLsFailsFastOnUnreachableBroker(t *testing.T) {
	ctx := context.Background()
	client, err := sqs.NewClient(ctx, unreachableConfig(), slog.Default())
	require.NoError(t, err)

	urls, err := sqs.ResolveQueueURLs(ctx, client, unreachableConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reachable",
		"the failure must name the queue so startup logs point at the cause")
	assert.Empty(t, urls.Request)
	assert.Empty(t, urls.Events)
	assert.Empty(t, urls.DLQ)
}

// TestNewClientBuildsAgainstAnyEndpoint pins that client construction itself is
// offline-safe: only the queue resolution dials, so a missing broker never
// fails at NewClient.
func TestNewClientBuildsAgainstAnyEndpoint(t *testing.T) {
	client, err := sqs.NewClient(context.Background(), unreachableConfig(), slog.Default())
	require.NoError(t, err)
	assert.NotNil(t, client)
}

// ── Happy path against an SQS-speaking stub ─────────────────────────────────

// TestResolveQueueURLsHappyPath completes the pair with the fail-fast test
// above: when the broker answers, every configured name must be resolved to
// the URL the broker returned — not to a hard-coded guess, and not skipped.
func TestResolveQueueURLsHappyPath(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			QueueName string `json:"QueueName"`
		}
		_ = json.Unmarshal(raw, &payload)
		got = append(got, payload.QueueName)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		fmt.Fprintf(w, `{"QueueUrl":"http://broker/%s"}`, payload.QueueName)
	}))
	defer srv.Close()

	cfg := unreachableConfig()
	cfg.SQSEndpoint = srv.URL
	client, err := sqs.NewClient(context.Background(), cfg, slog.Default())
	require.NoError(t, err)

	urls, err := sqs.ResolveQueueURLs(context.Background(), client, cfg)
	require.NoError(t, err)

	assert.Equal(t, "http://broker/wager-requests.fifo", urls.Request)
	assert.Equal(t, "http://broker/integration-events.fifo", urls.Events)
	assert.Equal(t, "http://broker/wager-dlq.fifo", urls.DLQ)
	assert.Equal(t, []string{"wager-requests.fifo", "integration-events.fifo", "wager-dlq.fifo"}, got,
		"each configured queue must be resolved exactly once, in order")
}
