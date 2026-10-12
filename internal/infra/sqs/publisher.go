package sqs

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/chalkan33/wallet/internal/app"
)

// Publisher publishes claimed outbox events to the integration events FIFO
// queue.
//
// Routing contract:
//   - MessageGroupId = aggregateId → per-aggregate FIFO ordering (events of
//     one wallet are delivered in order; different wallets parallelize);
//   - MessageDeduplicationId = eventId → SQS drops exact duplicates caused
//     by outbox at-least-once republication, while eventId remains stable.
type Publisher struct {
	client   *sqs.Client
	queueURL string
}

func NewPublisher(client *sqs.Client, urls QueueURLs) *Publisher {
	return &Publisher{client: client, queueURL: urls.Events}
}

// Publish sends one outbox item. Errors are returned to the worker, which
// schedules the retry — publication never happens before the originating
// transaction is committed (outbox pattern guarantee).
func (p *Publisher) Publish(ctx context.Context, item app.OutboxItem) error {
	_, err := p.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queueURL),
		MessageBody:            aws.String(string(item.Payload)),
		MessageGroupId:         aws.String(item.AggregateID),
		MessageDeduplicationId: aws.String(item.EventID.String()),
	})
	return err
}
