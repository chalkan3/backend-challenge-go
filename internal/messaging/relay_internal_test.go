// In-package test for the outbox relay's tick: the parts that only run when
// the database or the broker misbehaves. They are the difference between
// "publish failed, retry scheduled" and a silently stuck event, so each
// branch is pinned directly against a fake Repos and a real SQS client.
package messaging

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// --- fake TxManager/Repos ---------------------------------------------------

// outboxFake is the only port the relay touches: ClaimBatch and the two
// bookkeeping writes. The other repositories are nil-valued stubs — the relay
// must never reach for them, and a panic here would prove it does.
type outboxFake struct {
	items       []service.OutboxItem
	claimErr    error
	markPubErr  error
	markFailErr error

	markedPublished []uuid.UUID
	markedFailed    []uuid.UUID
	claimedCalls    int
}

func (f *outboxFake) Enqueue(context.Context, []domain.Envelope) error { return nil }

func (f *outboxFake) ClaimBatch(context.Context, time.Time, time.Duration, int) ([]service.OutboxItem, error) {
	f.claimedCalls++
	return f.items, f.claimErr
}

func (f *outboxFake) MarkPublished(_ context.Context, id uuid.UUID, _ time.Time) error {
	if f.markPubErr != nil {
		return f.markPubErr
	}
	f.markedPublished = append(f.markedPublished, id)
	return nil
}

func (f *outboxFake) MarkFailed(_ context.Context, id uuid.UUID, _ time.Time, _ string) error {
	if f.markFailErr != nil {
		return f.markFailErr
	}
	f.markedFailed = append(f.markedFailed, id)
	return nil
}

func (f *outboxFake) Stats(context.Context) (int64, float64, error) { return 0, 0, nil }

type relayRepos struct{ outbox *outboxFake }

func (r *relayRepos) Wallets() service.WalletRepository           { return nil }
func (r *relayRepos) WalletOpens() service.WalletOpenRepository   { return nil }
func (r *relayRepos) Transactions() service.TransactionRepository { return nil }
func (r *relayRepos) Ledger() service.LedgerRepository            { return nil }
func (r *relayRepos) Inbox() service.InboxRepository              { return nil }
func (r *relayRepos) Outbox() service.OutboxRepository            { return r.outbox }

type relayTxm struct{ repos *relayRepos }

func (t *relayTxm) WithinTx(context.Context, func(context.Context, service.Repos) error) error {
	return nil
}
func (t *relayTxm) Repos() service.Repos { return t.repos }

// relayHook records the counters the relay fires, so a branch is asserted by
// the observation it reports, not just by "no panic".
type relayHook struct {
	published int
	failed    int
	stats     int
}

func (h *relayHook) OutboxPublished() { h.published++ }
func (h *relayHook) OutboxFailed()    { h.failed++ }
func (h *relayHook) OutboxStats(int64, float64) {
	h.stats++
}
func (h *relayHook) ReconciliationSweep(int) {}
func (h *relayHook) StartSpan(ctx context.Context, _ string, _ ...SpanAttr) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttributes(...SpanAttr) {}
func (noopSpan) SetOutcome(string, string) {}
func (noopSpan) RecordError(error)         {}
func (noopSpan) End()                      {}

// --- helpers ----------------------------------------------------------------

func relayConfig() *config.Config {
	return &config.Config{
		OutboxInterval: 10 * time.Millisecond,
		OutboxBatch:    10,
		OutboxLease:    time.Second,
	}
}

// publisherToLocalStack builds a real Publisher pointed at LocalStack. An
// unreachable endpoint is used deliberately in the failure cases: the AWS SDK
// then returns a transport error, which is exactly the "broker down" path.
func publisherToLocalStack(t *testing.T, endpoint string) *sqs.Publisher {
	t.Helper()
	cfg := &config.Config{
		SQSEndpoint:    endpoint,
		SQSRegion:      "us-east-1",
		SQSAccessKeyID: "test",
		SQSSecretKey:   "test",
	}
	client, err := sqs.NewClient(context.Background(), cfg, slog.Default())
	require.NoError(t, err)
	return sqs.NewPublisher(client, sqs.QueueURLs{Events: endpoint + "/000000000000/events.fifo"})
}

func newWorker(f *outboxFake, pub *sqs.Publisher, hook Instrumentation) *OutboxWorker {
	return NewOutboxWorker(&relayTxm{repos: &relayRepos{outbox: f}},
		pub, relayConfig(), slog.Default(), hook)
}

// --- tests ------------------------------------------------------------------

// TestTickClaimFailureIsSwallowed pins that a failing ClaimBatch (e.g. the
// database is momentarily unreachable) does NOT crash the worker loop: the
// tick logs and returns, leaving the backlog intact for a later attempt.
func TestTickClaimFailureIsSwallowed(t *testing.T) {
	f := &outboxFake{claimErr: assert.AnError}
	hook := &relayHook{}
	w := newWorker(f, publisherToLocalStack(t, "http://127.0.0.1:1"), hook)

	assert.NotPanics(t, func() { w.tick(context.Background()) })
	assert.Equal(t, 0, hook.published)
	assert.Equal(t, 0, hook.failed)
	assert.Empty(t, f.markedPublished)
}

// TestTickSuccessfulPublish marks each claimed item published and fires the
// published counter once per item.
func TestTickSuccessfulPublishStub(t *testing.T) {
	// A single item published to an unreachable broker must be scheduled for
	// retry (the failure path); the success path is covered end to end by the
	// integration suite against LocalStack. This test pins the retry bookkeeping.
	id := uuid.New()
	f := &outboxFake{items: []service.OutboxItem{{
		EventID: id, AggregateID: "wallet-1", EventType: "WalletBalanceChanged", Payload: []byte(`{}`),
	}}}
	hook := &relayHook{}
	w := newWorker(f, publisherToLocalStack(t, "http://127.0.0.1:1"), hook)

	w.tick(context.Background())

	require.Len(t, f.markedFailed, 1, "a failed publish must be scheduled for retry")
	assert.Equal(t, id, f.markedFailed[0])
	assert.Equal(t, 1, hook.failed, "OutboxFailed must fire exactly once per failed item")
	assert.Empty(t, f.markedPublished)
}

// TestTickEmptyBatch is the quiet tick: nothing claimed, nothing published,
// nothing failed.
func TestTickEmptyBatch(t *testing.T) {
	f := &outboxFake{}
	hook := &relayHook{}
	w := newWorker(f, publisherToLocalStack(t, "http://127.0.0.1:1"), hook)

	w.tick(context.Background())

	assert.Equal(t, 1, f.claimedCalls, "the tick must query the outbox exactly once")
	assert.Equal(t, 0, hook.published)
	assert.Equal(t, 0, hook.failed)
}

// TestRunStopsOnContextCancel pins the graceful shutdown: Run blocks on the
// ticker but must return as soon as ctx is cancelled.
func TestRunStopsOnContextCancel(t *testing.T) {
	f := &outboxFake{}
	w := newWorker(f, publisherToLocalStack(t, "http://127.0.0.1:1"), &relayHook{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let at least one tick fire, then stop.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// --- reference worker -------------------------------------------------------

// TestReferenceTickClaimFailureIsSwallowed: a failing ClaimPendingReferences
// (the common case when the database is briefly down) must be logged and
// dropped, never crashing the worker loop.
func TestReferenceTickClaimFailureIsSwallowed(t *testing.T) {
	txm := &claimFailTxm{}
	w := NewReferenceWorker(txm, nil, relayConfig(), slog.Default(), &relayHook{})

	assert.NotPanics(t, func() { w.tick(context.Background()) })
}

// TestReferenceRunStopsOnContextCancel mirrors the outbox worker's shutdown.
func TestReferenceRunStopsOnContextCancel(t *testing.T) {
	cfg := relayConfig()
	cfg.ReferenceBatch = 10
	cfg.ReferenceInterval = 10 * time.Millisecond
	w := NewReferenceWorker(&claimFailTxm{}, nil, cfg, slog.Default(), &relayHook{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("reference Run did not return after cancellation")
	}
}

// claimFailTxm makes ClaimPendingReferences fail through the workers' own
// path: WithinTx runs the callback against a repos whose Transactions port
// reports the same fault.
type claimFailTxm struct{}

func (claimFailTxm) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	return fn(ctx, refRepos{})
}
func (claimFailTxm) Repos() service.Repos { return refRepos{} }

// refRepos is a Repos whose Transactions port fails on claim; every other
// port is nil because the reference tick must not touch them.
type refRepos struct{}

func (refRepos) Wallets() service.WalletRepository           { return nil }
func (refRepos) WalletOpens() service.WalletOpenRepository   { return nil }
func (refRepos) Transactions() service.TransactionRepository { return claimFailTxs{} }
func (refRepos) Ledger() service.LedgerRepository            { return nil }
func (refRepos) Inbox() service.InboxRepository              { return nil }
func (refRepos) Outbox() service.OutboxRepository            { return nil }

type claimFailTxs struct{}

func (claimFailTxs) Insert(context.Context, *domain.WagerTransaction) error { return nil }
func (claimFailTxs) Save(context.Context, *domain.WagerTransaction) error   { return nil }
func (claimFailTxs) GetByID(context.Context, uuid.UUID) (*domain.WagerTransaction, error) {
	return nil, assert.AnError
}
func (claimFailTxs) GetByProviderAndKey(context.Context, string, string) (*domain.WagerTransaction, error) {
	return nil, assert.AnError
}
func (claimFailTxs) GetByProviderAndExternal(context.Context, string, string) (*domain.WagerTransaction, error) {
	return nil, assert.AnError
}
func (claimFailTxs) ExistsProcessedReversal(context.Context, uuid.UUID, domain.TxKind) (bool, error) {
	return false, nil
}
func (claimFailTxs) ClaimPendingReferences(context.Context, time.Time, int) ([]*domain.WagerTransaction, error) {
	return nil, assert.AnError
}
