#!/usr/bin/env python3
"""Provisioning of SQS queues in LocalStack (runs once on container ready)."""
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


def arn_of(url: str) -> str:
    attrs = sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["QueueArn"])
    return attrs["Attributes"]["QueueArn"]


dlq = sqs.create_queue(
    QueueName="wager-transactions-dlq.fifo",
    Attributes={
        "FifoQueue": "true",
        "ContentBasedDeduplication": "true",
        "VisibilityTimeout": "60",
    },
)["QueueUrl"]

sqs.create_queue(
    QueueName="wager-transactions.fifo",
    Attributes={
        "FifoQueue": "true",
        "ContentBasedDeduplication": "true",
        "VisibilityTimeout": "60",
        "RedrivePolicy": json.dumps(
            {"deadLetterTargetArn": arn_of(dlq), "maxReceiveCount": 5}
        ),
    },
)

sqs.create_queue(
    QueueName="integration-events.fifo",
    Attributes={"FifoQueue": "true", "ContentBasedDeduplication": "false"},
)

names = [q for page in sqs.get_paginator("list_queues").paginate() for q in page.get("QueueUrls", [])]
print("[localstack-init] queues ready:", names)
