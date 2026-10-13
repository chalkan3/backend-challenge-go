package sqs

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// TraceParentAttribute is the SQS message attribute that carries the W3C
// traceparent from the publisher to the consumer, bridging the outbox gap.
const TraceParentAttribute = "traceparent"

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
//
// Trace continuity: the item carries the traceparent captured at enqueue time;
// it travels as a SQS message attribute (transport metadata, not domain data —
// the payload body stays the pure envelope) so the consumer can resume the
// originating trace. An empty value is simply not attached: the consumer then
// starts a fresh trace.
func (p *Publisher) Publish(ctx context.Context, item service.OutboxItem) error {
	in := &sqs.SendMessageInput{
		QueueUrl:               aws.String(p.queueURL),
		MessageBody:            aws.String(string(item.Payload)),
		MessageGroupId:         aws.String(item.AggregateID),
		MessageDeduplicationId: aws.String(item.EventID.String()),
	}
	if item.TraceParent != "" {
		in.MessageAttributes = map[string]awssqstypes.MessageAttributeValue{
			TraceParentAttribute: {
				DataType:    aws.String("String"),
				StringValue: aws.String(item.TraceParent),
			},
		}
	}
	_, err := p.client.SendMessage(ctx, in)
	return err
}
