package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/chalkan33/wallet/internal/app"
	"github.com/chalkan33/wallet/internal/domain"
	infraqs "github.com/chalkan33/wallet/internal/infra/sqs"
	"github.com/chalkan33/wallet/internal/platform/config"
	"github.com/chalkan33/wallet/internal/platform/logging"
)

const consumerName = "sqs-wager-consumer"

// Consumer consumes wager-transactions.fifo with at-least-once delivery.
//
// Durability protocol (per message):
//  1. parse envelope + hash the raw body;
//  2. in ONE SQL transaction: inbox row + domain effects + outbox events;
//  3. commit → delete the message. A crash before step 3 leaves the message
//     invisible until the visibility timeout; the committed inbox row makes
//     the redelivery a no-op. A crash before commit rolls everything back
//     and the redelivery processes normally.
//
// Terminal business outcomes (REJECTED results, conflicts) are terminal:
// the message is removed. Transient failures release the message for
// redelivery; after WALLET_SQS_MAX_RECEIVE receives the redrive policy (or
// the consumer itself) moves it to the DLQ.
type Consumer struct {
	client *sqs.Client
	urls   infraqs.QueueURLs
	cfg    *config.Config
	txm    app.TxManager
	svc    *app.WagerService
	hook   ConsumerMetrics
	log    *slog.Logger
}

type ConsumerMetrics interface {
	Received()
	Duplicate(source string)
	DLQ(reason string)
	Retry()
	Invalid(reason string)
}

func NewConsumer(client *sqs.Client, urls infraqs.QueueURLs, cfg *config.Config, txm app.TxManager, svc *app.WagerService, hook ConsumerMetrics, log *slog.Logger) *Consumer {
	return &Consumer{client: client, urls: urls, cfg: cfg, txm: txm, svc: svc, hook: hook,
		log: log.With(slog.String("worker", "consumer"))}
}

// sqsEnvelope mirrors the published ingress contract.
type sqsEnvelope struct {
	MessageID  string          `json:"messageId"`
	Type       string          `json:"type"`
	OccurredAt string          `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// poison marks a message as permanently unprocessable.
type poison struct{ Reason string }

func (e *poison) Error() string { return "poison message: " + e.Reason }

func (c *Consumer) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.cfg.ConsumerConcurrency)
	log := c.log
	log.Info("consumer started",
		slog.String("queue", c.cfg.SQSRequestQueue),
		slog.Int("concurrency", c.cfg.ConsumerConcurrency))

intake:
	for ctx.Err() == nil {
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.urls.Request),
			MaxNumberOfMessages: c.cfg.SQSMaxMessages,
			WaitTimeSeconds:     c.cfg.SQSWaitSeconds,
			VisibilityTimeout:   c.cfg.SQSVisibility,
			AttributeNames: []awssqstypes.QueueAttributeName{
				awssqstypes.QueueAttributeName("ApproximateReceiveCount"),
			},
		})
		if err != nil {
			if ctx.Err() != nil {
				break intake
			}
			log.Warn("receive failed; backing off", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				break intake
			case <-time.After(time.Second):
			}
			continue
		}
		for _, m := range out.Messages {
			msg := m
			select {
			case <-ctx.Done():
				break intake
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				c.handle(ctx, msg)
			}()
		}
	}

	// Graceful drain: stop intake, wait for in-flight work within the grace
	// period. Messages whose handling cannot finish lose visibility and are
	// safely redelivered (inbox + idempotency make that a no-op).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		log.Info("consumer stopped cleanly")
	case <-time.After(c.cfg.ShutdownGrace):
		log.Warn("consumer shutdown grace elapsed; in-flight work released for redelivery")
	}
	return nil
}

func (c *Consumer) handle(ctx context.Context, m awssqstypes.Message) {
	c.hook.Received()
	body := aws.ToString(m.Body)
	messageID := aws.ToString(m.MessageId)

	var envelope sqsEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		c.fail(ctx, m, &poison{Reason: "unparseable envelope: " + err.Error()})
		return
	}
	// The envelope messageId is the durable identity (survives redelivery);
	// the SQS MessageId is only a fallback.
	if envelope.MessageID != "" {
		messageID = envelope.MessageID
	}
	ctx = logging.WithCorrelation(ctx, messageID)
	ctx = logging.WithMessage(ctx, messageID)
	log := c.log.With(slog.String("messageId", messageID))

	hash := sha256Hex(body)

	if envelope.Type != domain.EventWagerTransactionRequested {
		c.fail(ctx, m, &poison{Reason: "unknown event type " + envelope.Type})
		return
	}
	var data domain.WagerTransactionRequestedData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		c.fail(ctx, m, &poison{Reason: "unparseable data: " + err.Error()})
		return
	}
	input := app.ExternalInput{
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
		CorrelationID:                  messageID,
	}
	t, err := c.svc.Prepare(input)
	if err != nil {
		c.fail(ctx, m, &poison{Reason: "business shape invalid: " + err.Error()})
		return
	}
	log = log.With(
		slog.String("transactionId", t.ID().String()),
		slog.String("walletId", t.WalletID().String()),
		slog.String("providerId", t.ProviderID()))

	err = c.txm.WithinTx(ctx, func(ctx context.Context, r app.Repos) error {
		outcome, err := r.Inbox().Begin(ctx, consumerName, messageID, hash)
		if err != nil {
			return err
		}
		if !outcome.Inserted {
			c.hook.Duplicate("inbox_redelivery")
			if !outcome.HashMatch {
				return &poison{Reason: "messageId reused with different content"}
			}
			if !outcome.Completed {
				log.Warn("inbox row found without completion (anomalous); skipping")
			}
			return nil // already durably handled → ack
		}
		_, err = c.svc.ExecuteInTx(ctx, r, t)
		if err != nil {
			return err
		}
		// completion is committed atomically with the domain effects: a
		// committed inbox row always carries completed_at
		return r.Inbox().Complete(ctx, consumerName, messageID, time.Now().UTC())
	})
	switch {
	case err == nil:
		c.ack(ctx, m, log)
	case errors.Is(err, app.ErrDuplicateInsert):
		// Lost an insert race against a concurrent delivery of the same
		// operation; the committed row wins, this delivery just acks.
		c.ack(ctx, m, log)
	case isPoison(err):
		c.fail(ctx, m, err)
	case app.IsValidation(err) || app.IsConflict(err) || app.IsNotFound(err):
		// Terminal anomalies: the message can never succeed as-is.
		c.fail(ctx, m, &poison{Reason: err.Error()})
	default:
		c.transient(ctx, m, err, log)
	}
}

func (c *Consumer) ack(ctx context.Context, m awssqstypes.Message, log *slog.Logger) {
	if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.urls.Request),
		ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		log.Warn("delete after commit failed; redelivery will hit the inbox",
			slog.String("error", err.Error()))
	}
}

func (c *Consumer) transient(ctx context.Context, m awssqstypes.Message, cause error, log *slog.Logger) {
	receiveCount := 1
	if rc, ok := m.Attributes[string(awssqstypes.QueueAttributeName("ApproximateReceiveCount"))]; ok {
		if n, err := strconv.Atoi(rc); err == nil {
			receiveCount = n
		}
	}
	if receiveCount >= int(c.cfg.SQSMaxReceive) {
		log.Error("max receive count exhausted; moving to DLQ",
			slog.Int("receiveCount", receiveCount), slog.String("error", cause.Error()))
		c.fail(ctx, m, &poison{Reason: "retries exhausted: " + cause.Error()})
		return
	}
	c.hook.Retry()
	log.Warn("transient failure; releasing message for redelivery",
		slog.Int("receiveCount", receiveCount), slog.String("error", cause.Error()))
	// Do NOT delete: the visibility timeout returns the message to the queue.
}

func (c *Consumer) fail(ctx context.Context, m awssqstypes.Message, cause error) {
	c.hook.DLQ("poison")
	c.hook.Invalid(cause.Error())
	c.log.Error("moving message to DLQ",
		slog.String("messageId", aws.ToString(m.MessageId)),
		slog.String("reason", cause.Error()))
	group := aws.ToString(m.MessageId)
	if _, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.urls.DLQ),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: aws.String(sha256Hex(aws.ToString(m.MessageId) + aws.ToString(m.Body))),
	}); err != nil {
		c.log.Error("DLQ send failed; message will be retried", slog.String("error", err.Error()))
		return
	}
	if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.urls.Request),
		ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		c.log.Error("delete after DLQ failed", slog.String("error", err.Error()))
	}
}

func isPoison(err error) bool {
	var p *poison
	return errors.As(err, &p)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
