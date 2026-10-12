// Package sqs provides the AWS SQS client for LocalStack/AWS plus the
// publisher used by the transactional outbox worker.
package sqs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

// NewClient builds an SQS client pointed at the configured endpoint
// (LocalStack locally, AWS in production).
func NewClient(ctx context.Context, cfg *config.Config, log *slog.Logger) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.SQSRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.SQSAccessKeyID, cfg.SQSSecretKey, "")),
	)
	if err != nil {
		return nil, err
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(cfg.SQSEndpoint)
	})
	log.Info("SQS client configured",
		slog.String("endpoint", cfg.SQSEndpoint), slog.String("region", cfg.SQSRegion))
	return client, nil
}

// QueueURLs resolves every queue URL at startup — fail fast when the broker
// is unreachable or a queue was not provisioned.
type QueueURLs struct {
	Request string
	Events  string
	DLQ     string
}

func ResolveQueueURLs(ctx context.Context, client *sqs.Client, cfg *config.Config) (QueueURLs, error) {
	var out QueueURLs
	var err error
	if out.Request, err = resolve(ctx, client, cfg.SQSRequestQueue); err != nil {
		return out, err
	}
	if out.Events, err = resolve(ctx, client, cfg.SQSEventsQueue); err != nil {
		return out, err
	}
	if out.DLQ, err = resolve(ctx, client, cfg.SQSDLQQueue); err != nil {
		return out, err
	}
	return out, nil
}

func resolve(ctx context.Context, client *sqs.Client, name string) (string, error) {
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return "", fmt.Errorf("sqs: queue %q not reachable: %w", name, err)
	}
	return *out.QueueUrl, nil
}
