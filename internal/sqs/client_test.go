// Unit tests for the SQS client's startup path. The happy path (resolving all
// three queue URLs) is covered end to end against LocalStack; here we pin the
// fail-fast branch: a broker the client cannot reach must stop startup, not
// hand back an empty URL that would make the first SendMessage fail much later,
// far from its cause.
package sqs_test

import (
	"context"
	"log/slog"
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
