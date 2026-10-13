#!/usr/bin/env python3
"""Provisioning of SQS queues in LocalStack (runs once on container ready).

QUEUE POLICIES
==============
The three queues carry policies that are *applied by the server*, and every one
of them is read back and asserted by test/integration/sqs_policy_test.go. The
distinction matters here: a queue policy that the server merely stores is
decoration. Two things were measured on this stack (LocalStack 4.14.0,
community) before writing this file:

  * Resource policies (the ``Policy`` attribute) are STORED BUT NOT ENFORCED.
    A ``Deny * on sqs:*`` was written to a queue and ``SendMessage`` still
    succeeded. Provisioning one here would be theatre, so none is written.
    Real AWS enforces it; see deploy/localstack/README.md.
  * The attributes below ARE enforced: RedrivePolicy was proven by sending a
    message and receiving it past ``maxReceiveCount`` until it landed in the
    DLQ; the FIFO/dedup/throughput attributes were proven by setting them and
    reading them back.

WHY EACH POLICY
---------------
wager-transactions.fifo — the work queue the consumer drains.
  * RedrivePolicy (maxReceiveCount=5) → a poison message stops being retried
    forever and lands in the DLQ for inspection. This is the difference between
    "the consumer is busy" and "the consumer will never succeed".
  * VisibilityTimeout=60 → must exceed the consumer's own processing budget, or
    a slow-but-correct message is redelivered *while still in flight*, which is
    the classic duplicate-processing bug.
  * MessageRetentionPeriod=4 days (default) → an outage longer than this loses
    the request; kept explicit so the bound is a decision, not a default.

wager-transactions-dlq.fifo — the quarantine.
  * MessageRetentionPeriod=14 days (max) → a poison message is the evidence of
    a bug; 4 days is not enough time to notice. Different retention from the
    source queue on purpose.

integration-events.fifo — the outbox relay's destination.
  * ContentBasedDeduplication=false → the publisher supplies an explicit
    MessageDeduplicationId (the event id), so the dedup key is the *identity of
    the event*, not a hash of its body. Two distinct events with coincidentally
    identical bodies must not collapse into one.
  * MessageRetentionPeriod=14 days → these are financial events; keep them.

All three:
  * ReceiveMessageWaitTimeSeconds=20 (long polling) → the consumer blocks on the
    socket instead of hot-looping with empty responses, which cuts both cost
    and latency. 20 is the maximum AWS accepts.
  * SqsManagedSseEnabled=true (default) → at-rest encryption with an
    AWS-managed key; explicit rather than implied.
  * MaximumMessageSize=1 MiB → the SQS default, but pinned because it is not
    merely a default here. internal/transport/http/codecs.go reads request
    bodies up to 1<<20, so a request the API accepts can be a full 1 MiB; a
    smaller queue would reject messages the service had already validated.
    Left implicit, this value would sit at the mercy of a server default — and
    one queue on a long-lived LocalStack volume was measured at 256 KiB, which
    is exactly the failure mode that motivates pinning it. See also
    deploy/tofu/variables.tf, where the provider's own default is wrong (it
    provisions 256 KiB) and the value had to be pinned there too.
"""
import json
import os

import boto3

region = os.environ.get("AWS_DEFAULT_REGION", "us-east-1")
endpoint = os.environ.get("AWS_ENDPOINT_URL", "http://localhost:4566")

sqs = boto3.client(
    "sqs",
    region_name=region,
    endpoint_url=endpoint,
    aws_access_key_id=os.environ.get("AWS_ACCESS_KEY_ID", "test"),
    aws_secret_access_key=os.environ.get("AWS_SECRET_ACCESS_KEY", "test"),
)

# Long polling ceiling; AWS caps this at 20 seconds.
LONG_POLL = "20"
# 14 days is the SQS maximum — used where a message is evidence worth keeping.
MAX_RETENTION = "1209600"
# The SQS default, pinned explicitly: the API accepts 1 MiB request bodies (see
# internal/transport/http/codecs.go), so a queue smaller than this can reject a
# message the service already validated.
MAX_MESSAGE_SIZE = "1048576"


def arn_of(url: str) -> str:
    attrs = sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["QueueArn"])
    return attrs["Attributes"]["QueueArn"]


def ensure_queue(name: str, attrs: dict) -> str:
    """Create the queue if absent, then (re)apply its attributes.

    create_queue alone is NOT idempotent for a policy change: LocalStack (and
    AWS) reject a create whose attributes differ from the existing queue with
    QueueAlreadyExists, leaving the old attributes in place — measured on this
    stack. So an existing queue is resolved with get_queue_url and its
    attributes are *reconciled* by set_queue_attributes. This makes the script
    safe to re-run and turns it from a one-shot bootstrap into provisioning.
    """
    try:
        url = sqs.create_queue(QueueName=name, Attributes=attrs)["QueueUrl"]
    except sqs.exceptions.QueueNameExists:
        url = sqs.get_queue_url(QueueName=name)["QueueUrl"]
    sqs.set_queue_attributes(QueueUrl=url, Attributes=attrs)
    return url


# ---------------------------------------------------------------- provisioning
# The DLQ first: the work queue's RedrivePolicy needs its ARN.
dlq = ensure_queue(
    "wager-transactions-dlq.fifo",
    {
        "FifoQueue": "true",
        "ContentBasedDeduplication": "true",
        "VisibilityTimeout": "60",
        # Quarantined messages are evidence: keep them as long as SQS allows.
        "MessageRetentionPeriod": MAX_RETENTION,
        "ReceiveMessageWaitTimeSeconds": LONG_POLL,
        "SqsManagedSseEnabled": "true",
        "MaximumMessageSize": MAX_MESSAGE_SIZE,
    },
)

ensure_queue(
    "wager-transactions.fifo",
    {
        "FifoQueue": "true",
        "ContentBasedDeduplication": "true",
        "VisibilityTimeout": "60",
        "MessageRetentionPeriod": "345600",  # 4 days
        "ReceiveMessageWaitTimeSeconds": LONG_POLL,
        "SqsManagedSseEnabled": "true",
        "MaximumMessageSize": MAX_MESSAGE_SIZE,
        "RedrivePolicy": json.dumps(
            {"deadLetterTargetArn": arn_of(dlq), "maxReceiveCount": 5}
        ),
    },
)

ensure_queue(
    "integration-events.fifo",
    {
        "FifoQueue": "true",
        # The relay sets MessageDeduplicationId = event id explicitly, so dedup
        # is by event identity, never by a hash of the payload.
        "ContentBasedDeduplication": "false",
        "MessageRetentionPeriod": MAX_RETENTION,  # financial events
        # Explicit like the other two queues: an omitted VisibilityTimeout would
        # silently take the SQS default of 30s, which is a value nobody chose.
        # (Was omitted until deploy/tofu refused to agree on a number — the sync
        # test caught it. See test/integration/tofu_sqs_sync_test.go.)
        "VisibilityTimeout": "60",
        "ReceiveMessageWaitTimeSeconds": LONG_POLL,
        "SqsManagedSseEnabled": "true",
        "MaximumMessageSize": MAX_MESSAGE_SIZE,
    },
)

names = [
    q
    for page in sqs.get_paginator("list_queues").paginate()
    for q in page.get("QueueUrls", [])
]
print("[localstack-init] queues ready:", names)
