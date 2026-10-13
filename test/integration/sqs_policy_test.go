//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// This file proves the SQS QUEUE POLICIES against the live broker, with the
// same discipline as the Keycloak policy test plus one addition specific to
// SQS on LocalStack:
//
//   - Queue attributes are read back and asserted. A policy the server does not
//     apply is not a policy, it is a comment.
//   - The redrive policy is proved *behaviourally*: a message is published,
//     received past maxReceiveCount, and observed to land in the DLQ. Reading
//     the attribute only proves it was stored.
//   - One test asserts the LIMIT of this environment honestly: a resource
//     policy (the "Policy" attribute) is accepted by LocalStack but NOT
//     enforced. That test documents the boundary instead of pretending.

// getAttrs reads every attribute SQS exposes for a named queue.
func getAttrs(t *testing.T, client *sqs.Client, name string) map[string]string {
	t.Helper()
	out, err := client.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(mustQueueURL(t, client, name)),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll},
	})
	if err != nil {
		t.Fatalf("get attributes %s: %v", name, err)
	}
	return out.Attributes
}

// TestSQSPoliciesAreApplied is the config half: every attribute the
// provisioning script writes must be visible on the running broker.
func TestSQSPoliciesAreApplied(t *testing.T) {
	client := mustSQS(t)

	t.Run("wager-transactions.fifo", func(t *testing.T) {
		attrs := getAttrs(t, client, "wager-transactions.fifo")
		assertAttr(t, attrs, "FifoQueue", "true")
		assertAttr(t, attrs, "VisibilityTimeout", "60")
		assertAttr(t, attrs, "ReceiveMessageWaitTimeSeconds", "20")
		assertAttr(t, attrs, "MessageRetentionPeriod", "345600")
		assertAttr(t, attrs, "ContentBasedDeduplication", "true")

		// The redrive policy must point at the DLQ with the agreed retry budget.
		var rp struct {
			DeadLetterTargetArn string `json:"deadLetterTargetArn"`
			MaxReceiveCount     any    `json:"maxReceiveCount"`
		}
		if err := json.Unmarshal([]byte(attrs["RedrivePolicy"]), &rp); err != nil {
			t.Fatalf("RedrivePolicy not parseable: %v (%q)", err, attrs["RedrivePolicy"])
		}
		if rp.DeadLetterTargetArn == "" {
			t.Errorf("RedrivePolicy has no deadLetterTargetArn (%q)", attrs["RedrivePolicy"])
		}
		if got := fmt.Sprint(rp.MaxReceiveCount); got != "5" {
			t.Errorf("RedrivePolicy.maxReceiveCount: got %s, want 5", got)
		}
		if want := "wager-transactions-dlq.fifo"; !hasSuffix(rp.DeadLetterTargetArn, want) {
			t.Errorf("RedrivePolicy target %q does not end in %s", rp.DeadLetterTargetArn, want)
		}
	})

	t.Run("wager-transactions-dlq.fifo", func(t *testing.T) {
		attrs := getAttrs(t, client, "wager-transactions-dlq.fifo")
		assertAttr(t, attrs, "FifoQueue", "true")
		assertAttr(t, attrs, "MessageRetentionPeriod", "1209600") // 14 days, the max
	})

	t.Run("integration-events.fifo", func(t *testing.T) {
		attrs := getAttrs(t, client, "integration-events.fifo")
		assertAttr(t, attrs, "FifoQueue", "true")
		// Dedup by explicit event id, never by payload hash.
		assertAttr(t, attrs, "ContentBasedDeduplication", "false")
		assertAttr(t, attrs, "MessageRetentionPeriod", "1209600")
		assertAttr(t, attrs, "ReceiveMessageWaitTimeSeconds", "20")
	})
}

// TestSQSRedrivePolicyIsEnforced is the behavioural half, and the reason this
// file exists: it proves the policy acts, not merely that it is stored. A
// message is published to the work queue, received past maxReceiveCount and
// then found in the DLQ.
func TestSQSRedrivePolicyIsEnforced(t *testing.T) {
	client := mustSQS(t)
	ctx := context.Background()
	src := mustQueueURL(t, client, "wager-transactions.fifo")
	dlq := mustQueueURL(t, client, "wager-transactions-dlq.fifo")

	body := `{"redriveProbe":"` + newPlayerID(t) + `"}`
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(src),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String("redrive-probe"),
		MessageDeduplicationId: aws.String("probe-" + newPlayerID(t)),
	}); err != nil {
		t.Fatalf("send probe: %v", err)
	}

	// Receive it past the retry budget. VisibilityTimeout makes each receive
	// immediately eligible again, so maxReceiveCount=5 is reached quickly.
	for i := 0; i < 5; i++ {
		if _, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(src),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     1,
		}); err != nil {
			t.Fatalf("receive %d: %v", i+1, err)
		}
	}

	// After the sixth delivery attempt the message must be in the DLQ.
	var found bool
	for i := 0; i < 20 && !found; i++ {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(dlq),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			t.Fatalf("receive dlq: %v", err)
		}
		for _, m := range out.Messages {
			if m.Body != nil && *m.Body == body {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("redrive policy NOT enforced: probe never reached the DLQ after maxReceiveCount deliveries")
	}
}

// TestSQSResourcePolicyIsAcceptedButNotEnforced documents the boundary of
// LocalStack community, measured before writing any provisioning: a resource
// policy saying "Deny everything" does not stop SendMessage. Real AWS enforces
// it (see deploy/localstack/README.md), so this test does two jobs at once —
// it keeps the claim honest and it will START FAILING if a future LocalStack
// begins enforcing policies, which is exactly when the provisioning should be
// revisited to add one.
func TestSQSResourcePolicyIsAcceptedButNotEnforced(t *testing.T) {
	client := mustSQS(t)
	ctx := context.Background()
	name := "integration-events.fifo"
	url := mustQueueURL(t, client, name)

	denyAll := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"sqs:*","Resource":"*"}]}`
	if _, err := client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(url),
		Attributes: map[string]string{"Policy": denyAll},
	}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	t.Cleanup(func() {
		// Never leave a policy behind: the next test must see a clean queue.
		_, _ = client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl:   aws.String(url),
			Attributes: map[string]string{"Policy": "{}"},
		})
	})

	// Stored…
	if attrs := getAttrs(t, client, name); attrs["Policy"] == "" || attrs["Policy"] == "{}" {
		t.Fatalf("policy was not stored; the probe is meaningless")
	}

	// …but not enforced: a Deny-all policy still lets us publish.
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(url),
		MessageBody:            aws.String(`{"policyProbe":true}`),
		MessageGroupId:         aws.String("policy-probe"),
		MessageDeduplicationId: aws.String("policy-probe-" + newPlayerID(t)),
	}); err != nil {
		t.Fatalf("resource policy IS enforced (%v); update the provisioning and this test", err)
	}
}

func assertAttr(t *testing.T, attrs map[string]string, key, want string) {
	t.Helper()
	if got := attrs[key]; got != want {
		t.Errorf("attribute %s: got %q, want %q", key, got, want)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
