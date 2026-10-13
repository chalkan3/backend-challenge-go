// In-package tests for the SQS consumer's handling protocol.
//
// The consumer is where at-least-once delivery meets exactly-once effects, so
// its branches are financial decisions, not plumbing:
//   - parse failure, wrong event type, invalid business shape → poison (DLQ);
//   - duplicate delivery → ack without moving money;
//   - transient failure → release for redelivery, unless retries are exhausted;
//   - success → ack (delete) only after the transaction committed.
//
// The SQS client is real (the AWS SDK pointed at an httptest server), not a
// mock: the code under test is exercised through the same call path the
// scheduler uses, including "did it actually call DeleteMessage?". Only the
// repository ports and the observation port are faked, because those are the
// collaborators the consumer is *defined* to talk to.
package messaging

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	sqsinfra "github.com/chalkan3/backend-challenge-go/internal/sqs"
)

// ── Fake repository ports ──────────────────────────────────────────────────
//
// The consumer touches Inbox plus whatever ExecuteInTx reaches. We give it an
// inbox we control and let the rest of the repositories be minimal: the
// success path is the one that needs the wager pipeline, and driving that
// through a fake store would test the service again, not the consumer. So the
// focus here is the decision tree — which branch runs for which error — with
// the transaction manager returning a scripted error or a scripted success.

type consumerInbox struct {
	beginOutcome service.InboxOutcome
	beginErr     error
	completeErr  error
	completed    []string
}

func (i *consumerInbox) Begin(context.Context, string, string, string) (service.InboxOutcome, error) {
	return i.beginOutcome, i.beginErr
}

func (i *consumerInbox) Complete(_ context.Context, _, messageID string, _ time.Time) error {
	if i.completeErr != nil {
		return i.completeErr
	}
	i.completed = append(i.completed, messageID)
	return nil
}

// fakeRepos serves exactly what the consumer's success path reaches: the
// idempotency lookup. We return an existing transaction with the SAME payload
// hash, so the service short-circuits to an idempotent replay — a genuine
// success that never needs the full wallet/ledger pipeline. The other
// repositories stay nil: the consumer must not reach for them on this path,
// and a nil dereference would prove it does.
type fakeRepos struct {
	inbox    *consumerInbox
	existing *domain.WagerTransaction
}

func (r *fakeRepos) Wallets() service.WalletRepository         { return nil }
func (r *fakeRepos) WalletOpens() service.WalletOpenRepository { return nil }
func (r *fakeRepos) Transactions() service.TransactionRepository {
	return &fakeTxRepo{existing: r.existing}
}
func (r *fakeRepos) Ledger() service.LedgerRepository    { return nil }
func (r *fakeRepos) Postings() service.PostingRepository { return nil }
func (r *fakeRepos) Inbox() service.InboxRepository      { return r.inbox }
func (r *fakeRepos) Outbox() service.OutboxRepository    { return nil }

// fakeTxRepo implements only the lookup the idempotency check performs.
type fakeTxRepo struct {
	service.TransactionRepository // nil embed: any other call panics on purpose
	existing                      *domain.WagerTransaction
	notFound                      bool
}

func (r *fakeTxRepo) GetByProviderAndKey(context.Context, string, string) (*domain.WagerTransaction, error) {
	if r.notFound || r.existing == nil {
		return nil, service.ErrRepoNotFound
	}
	return r.existing, nil
}

func (r *fakeTxRepo) GetByProviderAndExternal(context.Context, string, string) (*domain.WagerTransaction, error) {
	if r.notFound || r.existing == nil {
		return nil, service.ErrRepoNotFound
	}
	return r.existing, nil
}

// scriptedTxm drives WithinTx with a fixed outcome. When the callback runs and
// returns nil we report a clean commit; any other choice is the test telling
// the consumer to take a specific branch.
type scriptedTxm struct {
	repos   service.Repos
	txErr   error // error returned from WithinTx itself (transient/db failure)
	invoked bool
}

func (t *scriptedTxm) WithinTx(ctx context.Context, fn func(context.Context, service.Repos) error) error {
	t.invoked = true
	if t.txErr != nil {
		// The real manager would run the callback and roll back; a db-level
		// failure can surface before the callback, so we emulate the simplest
		// reachable shape: report the failure without a callback.
		return t.txErr
	}
	return fn(ctx, t.repos)
}
func (t *scriptedTxm) Repos() service.Repos { return t.repos }

// ── Fake observation port ──────────────────────────────────────────────────

type consumerHook struct {
	mu        sync.Mutex
	received  int
	duplicate int
	dlq       int
	retry     int
	invalid   int
	outcomes  []string
	spans     int
}

func (h *consumerHook) Received()        { h.mu.Lock(); h.received++; h.mu.Unlock() }
func (h *consumerHook) Duplicate(string) { h.mu.Lock(); h.duplicate++; h.mu.Unlock() }
func (h *consumerHook) DLQ(string)       { h.mu.Lock(); h.dlq++; h.mu.Unlock() }
func (h *consumerHook) Retry()           { h.mu.Lock(); h.retry++; h.mu.Unlock() }
func (h *consumerHook) Invalid(string)   { h.mu.Lock(); h.invalid++; h.mu.Unlock() }

// The service's Instrumentation port (the subset the consumer's fake needs to
// satisfy, so the WagerService it builds has a working observation sink).
func (h *consumerHook) WagerOutcome(string, string, string, string) {}
func (h *consumerHook) ReferenceOutcome(string)                     {}
func (h *consumerHook) ReconciliationDivergence()                   {}

type recSpan struct{ hook *consumerHook }

func (s *recSpan) SetAttributes(...SpanAttr) {}
func (s *recSpan) RecordError(error)         {}
func (s *recSpan) End()                      {}
func (s *recSpan) SetOutcome(status, _ string) {
	s.hook.mu.Lock()
	s.hook.outcomes = append(s.hook.outcomes, status)
	s.hook.mu.Unlock()
}

func (h *consumerHook) StartSpan(ctx context.Context, _ string, _ ...SpanAttr) (context.Context, Span) {
	h.mu.Lock()
	h.spans++
	h.mu.Unlock()
	return ctx, &recSpan{hook: h}
}

// extractingTracer records that Extract ran; it returns ctx unchanged, which
// is a valid "malformed carrier" response and keeps the test free of SDK.
type extractingTracer struct{ calls int }

func (t *extractingTracer) Extract(ctx context.Context, _ string) context.Context {
	t.calls++
	return ctx
}

// ── Test harness: a real SQS client against an httptest server ─────────────

// sqsStub records the API calls the consumer makes and replies minimally.
type sqsStub struct {
	mu       sync.Mutex
	deleted  []string
	dlqSends []string
	// deleteStatus lets a test force DeleteMessage to fail.
	deleteStatus int
	// messages are handed out one batch at a time by ReceiveMessage, so a
	// test can drive Run() through real deliveries instead of the empty path.
	messages [][]awssqstypes.Message
	// receiveStatus != 0 makes ReceiveMessage answer with an error, which is
	// how the intake back-off branch is reached.
	receiveStatus int
	// receiveCalls counts how many times ReceiveMessage was asked.
	receiveCalls int
}

func (s *sqsStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The AWS SDK v2 speaks the aws-json-1.0 protocol to SQS: the action is
		// in the X-Amz-Target header and the body is JSON. Modeling that (and
		// answering in JSON, not XML) is what makes this stub faithful; an
		// answer in the wrong dialect would surface as a confusing consumer
		// error, which is exactly what we spent a debug cycle on.
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			QueueUrl      string `json:"QueueUrl"`
			ReceiptHandle string `json:"ReceiptHandle"`
			MessageBody   string `json:"MessageBody"`
		}
		_ = json.Unmarshal(raw, &payload)
		action := r.Header.Get("X-Amz-Target") // e.g. AmazonSQS.DeleteMessage
		if action == "" {
			action = r.FormValue("Action")
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasSuffix(action, "ReceiveMessage"):
			s.receiveCalls++
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			if s.receiveStatus != 0 {
				w.WriteHeader(s.receiveStatus)
				_, _ = w.Write([]byte(`{"__type":"InternalError","message":"boom"}`))
				return
			}
			if len(s.messages) == 0 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			batch := s.messages[0]
			s.messages = s.messages[1:]
			body, _ := json.Marshal(map[string]any{"Messages": batch})
			_, _ = w.Write(body)
		case strings.HasSuffix(action, "DeleteMessage"):
			if s.deleteStatus != 0 {
				w.WriteHeader(s.deleteStatus)
				_, _ = w.Write([]byte(`{"__type":"InternalError","message":"boom"}`))
				return
			}
			s.deleted = append(s.deleted, payload.ReceiptHandle)
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(action, "SendMessage"):
			s.dlqSends = append(s.dlqSends, payload.MessageBody)
			// The SDK validates the echoed body checksum, so answer with the
			// real MD5 rather than a placeholder.
			sum := md5.Sum([]byte(payload.MessageBody))
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			fmt.Fprintf(w, `{"MessageId":"m","MD5OfMessageBody":"%x"}`, sum)
		default:
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			_, _ = w.Write([]byte(`{}`))
		}
	})
}

// ── Test harness: a real SQS client against an httptest server ─────────────

// newTestConsumer wires a Consumer with the given fakes and a real client
// pointed at the stub server.
func newTestConsumer(t *testing.T, stub *sqsStub, txm service.TxManager, hook *consumerHook, tracer TraceExtractor) *Consumer {
	t.Helper()

	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", ""),
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
	})

	cfg := &config.Config{
		SQSRequestQueue: "wager-transactions.fifo",
		SQSEventsQueue:  "integration-events.fifo",
		SQSDLQQueue:     "wager-transactions-dlq.fifo",
		SQSMaxReceive:   3,
		ShutdownGrace:   time.Second,
	}
	urls := sqsinfra.QueueURLs{
		Request: srv.URL + "/queue/request",
		Events:  srv.URL + "/queue/events",
		DLQ:     srv.URL + "/queue/dlq",
	}

	// A real service is unnecessary: Prepare only builds the aggregate from the
	// input, and every branch under test either fails before ExecuteInTx or
	// takes the duplicate/error path. We pass a service built on the same fakes.
	svc := service.NewWagerService(txm, slog.Default(), svcHook{})

	return NewConsumer(client, urls, cfg, txm, svc, hook, tracer, slog.Default())
}

// envelope builds a well-formed WagerTransactionRequested body.
func envelope(t *testing.T, mutate func(*domain.WagerTransactionRequestedData)) string {
	t.Helper()
	data := domain.WagerTransactionRequestedData{
		ProviderID:            "provider-a",
		ExternalTransactionID: "ext-1",
		IdempotencyKey:        "idem-1",
		PlayerID:              domain.MustNewID().String(),
		WalletID:              domain.MustNewID().String(),
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  "BET",
		Money:                 mustMoneyDTO(t, 10_00, "BRL"),
	}
	if mutate != nil {
		mutate(&data)
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)

	body, err := json.Marshal(sqsEnvelope{
		MessageID:  "msg-1",
		Type:       domain.EventWagerTransactionRequested,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
		Data:       raw,
	})
	require.NoError(t, err)
	return string(body)
}

func msg(body string) awssqstypes.Message {
	return awssqstypes.Message{
		MessageId:     aws.String("sqs-1"),
		Body:          aws.String(body),
		ReceiptHandle: aws.String("rh-1"),
		Attributes: map[string]string{
			string(awssqstypes.QueueAttributeName("ApproximateReceiveCount")): "1",
		},
	}
}

func mustMoneyDTO(t *testing.T, minor int64, currency string) domain.MoneyPayload {
	t.Helper()
	// MoneyPayload is the wire shape of Money: a decimal string plus a
	// currency. The consumer decodes it back into service.ExternalInput, which
	// re-parses the amount — so the string form is what must be exercised.
	m, err := domain.NewMoney(minor, currency)
	require.NoError(t, err)
	return domain.MoneyPayload{Amount: m.String(), Currency: currency}
}

// ── Tests ──────────────────────────────────────────────────────────────────

// TestConsumerHandleSuccessAcksAndCompletes is the happy path: a valid message
// runs the transaction, completes the inbox row and deletes the SQS message.
// If any of those three is missing, the protocol is broken — money would move
// twice (no ack) or never be marked done (no complete).
func TestConsumerHandleSuccessAcksAndCompletes(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	body := envelope(t, nil)
	// A stored transaction whose payload hash matches the message: the
	// idempotency check recognises the same operation and replays it.
	existing := replayedTxWithHash(t, body)
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox, existing: existing}}
	hook := &consumerHook{}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(body))

	assert.Equal(t, 1, hook.received, "the hook must see the message")
	require.Len(t, inbox.completed, 1, "the inbox row must be completed")
	assert.Equal(t, []string{"rh-1"}, stub.deleted, "the message must be acknowledged")
	assert.Contains(t, hook.outcomes, "PROCESSED")
}

// TestConsumerHandleDuplicateAcksWithoutReprocessing pins the exactly-once
// effect: a completed delivery is acknowledged and the wager use case is NOT
// run again.
//
// Note the granularity. The consumer always opens a transaction — that is
// where the inbox row lives — so txm.invoked being true is correct. What must
// not happen is the domain work: no new transaction is prepared, so
// fakeRepos.Transactions() is never reached. We assert on the observation the
// duplicate branch reports ("inbox_redelivery") plus the ack, which together
// are the contract.
func TestConsumerHandleDuplicateAcksWithoutReprocessing(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{
		Exists: true, HashMatch: true, Completed: true,
	}}
	// A repos with no existing transaction: if the duplicate branch wrongly
	// fell through to ExecuteInTx, the lookup would return NotFound and the
	// flow would diverge — the assertions below would catch it.
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}
	hook := &consumerHook{}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(envelope(t, nil)))

	assert.Equal(t, []string{"rh-1"}, stub.deleted, "a duplicate must be acked")
	assert.Equal(t, 1, hook.duplicate, "the duplicate counter must fire")
}

// TestConsumerHandleAnomalousInboxRowAcks covers the middle case: an inbox row
// exists with a matching hash but was never completed. It is an anomaly, and
// the safe move is to ack (the row already proves the work was attempted and
// the inbox is the source of truth) rather than loop forever.
func TestConsumerHandleAnomalousInboxRowAcks(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{
		Exists: true, HashMatch: true, Completed: false,
	}}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}

	c := newTestConsumer(t, stub, txm, &consumerHook{}, &extractingTracer{})
	c.handle(context.Background(), msg(envelope(t, nil)))

	assert.Equal(t, []string{"rh-1"}, stub.deleted)
}

// TestConsumerHandleRejectsMalformedBodies walks the three poison inputs. Each
// must reach the DLQ (fail) and must NOT be acked — the difference between
// "silently dropped" and "preserved for inspection".
func TestConsumerHandleRejectsMalformedBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unparseable envelope", "not json at all"},
		{"unknown event type", `{"messageId":"m","type":"SomethingElse","data":{}}`},
		{"unparseable data", `{"messageId":"m","type":"WagerTransactionRequested","data":"oops"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &sqsStub{}
			inbox := &consumerInbox{}
			txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}
			hook := &consumerHook{}

			c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
			c.handle(context.Background(), msg(tc.body))

			// A poison message is preserved for inspection, not dropped: it is
			// first copied to the DLQ, and only then removed from the source
			// queue. Deleting first would lose it if the DLQ send failed.
			require.Len(t, stub.dlqSends, 1, "the body must be forwarded to the DLQ")
			assert.Equal(t, aws.ToString(msg(tc.body).Body), stub.dlqSends[0])
			assert.Equal(t, []string{"rh-1"}, stub.deleted,
				"the source message is removed only after the DLQ copy")
			assert.Equal(t, 1, hook.dlq, "the DLQ counter must fire")
		})
	}
}

// TestConsumerHandleInvalidBusinessShapeReachesDLQ covers the Prepare failure:
// the JSON is fine, but the business shape is not (a bare UUID-less player, an
// invalid kind, a negative amount). It is terminal, so it goes to the DLQ.
func TestConsumerHandleInvalidBusinessShapeReachesDLQ(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}
	hook := &consumerHook{}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	// An unknown kind cannot be prepared: business shape is invalid.
	c.handle(context.Background(), msg(envelope(t, func(d *domain.WagerTransactionRequestedData) {
		d.Kind = "NOT_A_KIND"
	})))

	require.Len(t, stub.dlqSends, 1, "the body must be forwarded to the DLQ")
	assert.Equal(t, []string{"rh-1"}, stub.deleted,
		"the source message is removed only after the DLQ copy")
	assert.False(t, txm.invoked, "an unpreparable message must not open a transaction")
	assert.Equal(t, 1, hook.dlq, "an invalid business shape is terminal → DLQ")
}

// TestConsumerHandleTransientFailureReleasesForRedelivery is the retry branch:
// a database error while the retries remain must NOT delete the message and
// must NOT go to the DLQ — the visibility timeout brings it back.
func TestConsumerHandleTransientFailureReleasesForRedelivery(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}, txErr: errors.New("connection reset")}
	hook := &consumerHook{}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(envelope(t, nil)))

	assert.Empty(t, stub.deleted, "a transient failure must not ack")
	assert.Equal(t, 1, hook.retry, "the retry counter must move")
	assert.Contains(t, hook.outcomes, "RETRY")
}

// TestConsumerHandleExhaustedRetriesGoesToDLQ covers the other side of the
// transient branch: once ApproximateReceiveCount reaches the configured
// maximum, the message is poison and leaves the queue for the DLQ.
func TestConsumerHandleExhaustedRetriesGoesToDLQ(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}, txErr: errors.New("still failing")}
	hook := &consumerHook{}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	m := msg(envelope(t, nil))
	// SQSMaxReceive is 3 in the harness: this delivery is the last one.
	m.Attributes[string(awssqstypes.QueueAttributeName("ApproximateReceiveCount"))] = "3"

	c.handle(context.Background(), m)

	assert.Equal(t, 0, hook.retry, "an exhausted message is not retried, it is dropped to the DLQ")
	assert.Equal(t, 1, hook.dlq, "the DLQ counter must fire once retries are exhausted")
}

// TestConsumerHandleExtractsTheTraceParent proves the trace is resumed before
// the handle span opens: a message carrying the attribute must call Extract.
func TestConsumerHandleExtractsTheTraceParent(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	body := envelope(t, nil)
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox, existing: replayedTxWithHash(t, body)}}
	tracer := &extractingTracer{}

	c := newTestConsumer(t, stub, txm, &consumerHook{}, tracer)
	m := msg(body)
	m.MessageAttributes = map[string]awssqstypes.MessageAttributeValue{
		sqsinfra.TraceParentAttribute: {StringValue: aws.String("00-abc-def-01")},
	}
	c.handle(context.Background(), m)

	assert.Equal(t, 1, tracer.calls, "a traceparent attribute must be extracted exactly once")
}

// TestTraceParentOf pins the nil-tolerant contract directly, including the
// absent-attribute and nil-value cases that "start a new trace".
func TestTraceParentOf(t *testing.T) {
	assert.Equal(t, "", traceParentOf(awssqstypes.Message{}), "no attribute → empty")

	empty := awssqstypes.Message{MessageAttributes: map[string]awssqstypes.MessageAttributeValue{
		sqsinfra.TraceParentAttribute: {StringValue: nil},
	}}
	assert.Equal(t, "", traceParentOf(empty), "nil value → empty")

	ok := awssqstypes.Message{MessageAttributes: map[string]awssqstypes.MessageAttributeValue{
		sqsinfra.TraceParentAttribute: {StringValue: aws.String("tp")},
	}}
	assert.Equal(t, "tp", traceParentOf(ok))
}

// TestSha256Hex pins the hashing used for redelivery comparison: the same body
// must hash identically (otherwise every redelivery looks like a new payload)
// and different bodies must differ.
func TestSha256Hex(t *testing.T) {
	a := sha256Hex(`{"x":1}`)
	b := sha256Hex(`{"x":1}`)
	c := sha256Hex(`{"x":2}`)

	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.Len(t, a, 64, "sha256 hex is 64 characters")
}

// replayedTxWithHash builds a stored transaction whose payload hash equals the
// one the service computes for the given raw body. That makes the idempotency
// check classify the delivery as a replay, which is the shortest genuine
// success path through ExecuteInTx — no wallet or ledger needed.
func replayedTxWithHash(t *testing.T, body string) *domain.WagerTransaction {
	t.Helper()

	var env sqsEnvelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	var data domain.WagerTransactionRequestedData
	require.NoError(t, json.Unmarshal(env.Data, &data))

	amount, err := domain.ParseMoney(data.Money.Amount, data.Money.Currency)
	require.NoError(t, err)

	// The consumer builds ExternalInput with ReceivedVia=ViaSQS and
	// CorrelationID=messageId; the hash is computed over that same shape.
	input := service.ExternalInput{
		ProviderID:                     data.ProviderID,
		ExternalTransactionID:          data.ExternalTransactionID,
		IdempotencyKey:                 data.IdempotencyKey,
		PlayerID:                       data.PlayerID,
		WalletID:                       data.WalletID,
		RoundID:                        data.RoundID,
		GameID:                         data.GameID,
		Kind:                           data.Kind,
		Amount:                         data.Money.Amount,
		Currency:                       data.Money.Currency,
		ReferenceExternalTransactionID: data.ReferenceExternalTransactionID,
		ReceivedVia:                    domain.ViaSQS,
		CorrelationID:                  "msg-1",
	}
	hash, err := service.CanonicalHash(input, amount)
	require.NoError(t, err)

	tx := mustReplayTx(t, data)
	tx.SetIdempotency(data.IdempotencyKey, hash)
	return tx
}

// mustReplayTx builds a PROCESSED transaction carrying a balance, so
// replayResult returns without touching the wallet repository.
func mustReplayTx(t *testing.T, data domain.WagerTransactionRequestedData) *domain.WagerTransaction {
	t.Helper()

	playerID, err := domain.ParseID(data.PlayerID)
	require.NoError(t, err)
	walletID, err := domain.ParseID(data.WalletID)
	require.NoError(t, err)
	amount, err := domain.ParseMoney(data.Money.Amount, data.Money.Currency)
	require.NoError(t, err)

	before := int64(100_00)
	after := before - amount.AmountMinor()
	version := int64(2)
	hash := "placeholder"

	tx, err := domain.RehydrateWagerTransaction(domain.RehydrateParams{
		ID:                 domain.MustNewID(),
		Kind:               domain.KindBet,
		Origin:             domain.OriginExternal,
		PlayerID:           playerID,
		WalletID:           walletID,
		Currency:           data.Money.Currency,
		AmountMinor:        amount.AmountMinor(),
		PayloadHash:        &hash,
		Status:             domain.StatusProcessed,
		ResultDirection:    domain.DirectionDebit,
		ResultAmountMinor:  amount.AmountMinor(),
		BalanceBeforeMinor: &before,
		BalanceAfterMinor:  &after,
		WalletVersionAfter: &version,
		CorrelationID:      "msg-1",
		ReceivedVia:        domain.ViaSQS,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	})
	require.NoError(t, err)
	return tx
}

// ── Run: the intake loop ────────────────────────────────────────────────────

// runConfig fills the fields Run requires; the other tests build the consumer
// by hand and never start the loop, so they leave these at zero.
func runConfig() *config.Config {
	return &config.Config{
		SQSRequestQueue:     "wager-transactions.fifo",
		SQSEventsQueue:      "integration-events.fifo",
		SQSDLQQueue:         "wager-transactions-dlq.fifo",
		SQSMaxMessages:      10,
		SQSVisibility:       60,
		SQSWaitSeconds:      0,
		SQSMaxReceive:       3,
		ConsumerConcurrency: 4,
		ShutdownGrace:       time.Second,
	}
}

// newRunConsumer is newTestConsumer but with a config that lets Run start.
func newRunConsumer(t *testing.T, stub *sqsStub, txm service.TxManager, hook *consumerHook) *Consumer {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	client := sqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("k", "s", ""),
	}, func(o *sqs.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	urls := sqsinfra.QueueURLs{
		Request: srv.URL + "/queue/request",
		Events:  srv.URL + "/queue/events",
		DLQ:     srv.URL + "/queue/dlq",
	}
	svc := service.NewWagerService(txm, slog.Default(), svcHook{})
	return NewConsumer(client, urls, runConfig(), txm, svc, hook, &extractingTracer{}, slog.Default())
}

// TestConsumerRunDrainsDeliveredBatch drives the real poll loop: one batch is
// delivered, handled and acked, then the context is cancelled and Run returns
// cleanly. This is the path the whole service depends on at boot — if Run
// never consumed or never drained, the consumer would look alive while making
// no progress.
func TestConsumerRunDrainsDeliveredBatch(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	body := envelope(t, nil)
	existing := replayedTxWithHash(t, body)
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox, existing: existing}}
	hook := &consumerHook{}
	stub.messages = [][]awssqstypes.Message{{msg(body)}}

	c := newRunConsumer(t, stub, txm, hook)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Wait until the message has actually been handled, then stop the loop.
	deadline := time.After(5 * time.Second)
	for {
		stub.mu.Lock()
		acked := len(stub.deleted)
		stub.mu.Unlock()
		if acked > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run never acked the delivered message")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled Run drains and returns nil")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	assert.Equal(t, 1, hook.received)
	require.Len(t, inbox.completed, 1)
	assert.Equal(t, []string{"rh-1"}, stub.deleted)
}

// TestConsumerRunBacksOffOnReceiveError pins the intake branch that is easy to
// get wrong: a ReceiveMessage failure must not kill the loop. The service must
// keep polling — a transient SQS blip cannot be allowed to silently stop
// consumption — and only a cancelled context ends Run.
func TestConsumerRunBacksOffOnReceiveError(t *testing.T) {
	stub := &sqsStub{receiveStatus: http.StatusInternalServerError}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: &consumerInbox{}}}
	hook := &consumerHook{}

	c := newRunConsumer(t, stub, txm, hook)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Give the loop time to fail, back off (~1s) and try again: more than one
	// receive means it survived the error. The wait must exceed the 1s backoff
	// so the second (and third) attempt actually happen.
	deadline := time.After(8 * time.Second)
	for {
		stub.mu.Lock()
		calls := stub.receiveCalls
		stub.mu.Unlock()
		if calls >= 3 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("Run stopped polling after a receive error (calls=%d)", calls)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestConsumerAckSurvivesDeleteFailure pins the ack path's error branch: if
// DeleteMessage fails after the transaction committed, the consumer must log
// and carry on — never panic and never reprocess. The message will be
// redelivered and the inbox will recognise it; that is the whole reason the
// delete is allowed to fail here.
func TestConsumerAckSurvivesDeleteFailure(t *testing.T) {
	stub := &sqsStub{deleteStatus: http.StatusInternalServerError}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	body := envelope(t, nil)
	existing := replayedTxWithHash(t, body)
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox, existing: existing}}

	c := newTestConsumer(t, stub, txm, &consumerHook{}, &extractingTracer{})

	assert.NotPanics(t, func() { c.handle(context.Background(), msg(body)) },
		"a failing delete must be logged, not fatal")
	assert.Empty(t, stub.deleted, "the stub refused the delete, so nothing was recorded")
	require.Len(t, inbox.completed, 1, "the work still completed; only the ack failed")
}

// TestConsumerDLQDeletesTheMessage covers the terminal path end to end: a poison
// message is sent to the DLQ and only then deleted from the source queue. The
// order matters — deleting first would lose the message if the DLQ send failed.
func TestConsumerDLQDeletesTheMessage(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}, txErr: errors.New("still failing")}

	c := newTestConsumer(t, stub, txm, &consumerHook{}, &extractingTracer{})
	m := msg(envelope(t, nil))
	m.Attributes[string(awssqstypes.QueueAttributeName("ApproximateReceiveCount"))] = "3"

	c.handle(context.Background(), m)

	require.Len(t, stub.dlqSends, 1, "the poison body must reach the DLQ")
	assert.Equal(t, envelopeBody(t, m), stub.dlqSends[0], "the DLQ receives the original body verbatim")
	assert.Equal(t, []string{"rh-1"}, stub.deleted, "only after the DLQ send is the source message deleted")
}

// TestConsumerDLQDeleteFailureIsNotFatal pins the last error branch: a delete
// that fails after the DLQ send leaves the message to be redelivered, which
// the inbox tolerates. It must not panic or stop the worker.
func TestConsumerDLQDeleteFailureIsNotFatal(t *testing.T) {
	stub := &sqsStub{deleteStatus: http.StatusInternalServerError}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: true}}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}, txErr: errors.New("still failing")}

	c := newTestConsumer(t, stub, txm, &consumerHook{}, &extractingTracer{})
	m := msg(envelope(t, nil))
	m.Attributes[string(awssqstypes.QueueAttributeName("ApproximateReceiveCount"))] = "3"

	assert.NotPanics(t, func() { c.handle(context.Background(), m) })
	assert.Len(t, stub.dlqSends, 1, "the DLQ send still happened")
	assert.Empty(t, stub.deleted, "the refused delete recorded nothing")
}

func envelopeBody(t *testing.T, m awssqstypes.Message) string {
	t.Helper()
	return aws.ToString(m.Body)
}

// TestConsumerHandleInboxBeginFailureIsTransient pins the first branch inside
// the transaction: if the inbox row cannot even be written, nothing committed,
// so the message must be released for redelivery — not acked.
func TestConsumerHandleInboxBeginFailureIsTransient(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginErr: errors.New("inbox unavailable")}
	hook := &consumerHook{}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(envelope(t, nil)))

	assert.Equal(t, 1, hook.retry, "a failed inbox write is transient")
	assert.Empty(t, stub.deleted, "nothing committed, so nothing is acked")
	assert.Empty(t, stub.dlqSends)
}

// TestConsumerHandleRedeliveryWithDifferentBodyIsPoison: the same messageId
// arriving with different content is a protocol violation — the inbox key is
// (consumer, messageId), so accepting it would let one identity carry two
// operations. It must go to the DLQ, never be acked as a duplicate.
func TestConsumerHandleRedeliveryWithDifferentBodyIsPoison(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{beginOutcome: service.InboxOutcome{Inserted: false, HashMatch: false}}
	hook := &consumerHook{}
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox}}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(envelope(t, nil)))

	assert.Equal(t, 1, hook.dlq, "a reused messageId with new content is poison")
	assert.Equal(t, 1, hook.duplicate, "it is still counted as a duplicate delivery")
	require.Len(t, stub.dlqSends, 1, "the poison body reaches the DLQ")
	assert.Equal(t, []string{"rh-1"}, stub.deleted,
		"the source message is deleted only after the DLQ send")
}

// TestConsumerHandleLostInsertRaceAcksWithoutReprocessing: when the inbox
// insert loses a race (ErrDuplicateInsert), the winner's committed row is the
// truth. This delivery must ack and must NOT run the operation again.
func TestConsumerHandleLostInsertRaceAcksWithoutReprocessing(t *testing.T) {
	stub := &sqsStub{}
	inbox := &consumerInbox{
		beginOutcome: service.InboxOutcome{Inserted: true},
		completeErr:  service.ErrDuplicateInsert,
	}
	hook := &consumerHook{}
	// A stored transaction whose hash matches the body makes ExecuteInTx a
	// replay, so the path reaches the Complete step that loses the race.
	body := envelope(t, nil)
	txm := &scriptedTxm{repos: &fakeRepos{inbox: inbox, existing: replayedTxWithHash(t, body)}}

	c := newTestConsumer(t, stub, txm, hook, &extractingTracer{})
	c.handle(context.Background(), msg(body))

	assert.Equal(t, []string{"rh-1"}, stub.deleted,
		"losing the insert race is a normal outcome and the message must be acked")
	assert.Equal(t, 0, hook.retry, "it is not retried")
	assert.Equal(t, 0, hook.dlq)
}
