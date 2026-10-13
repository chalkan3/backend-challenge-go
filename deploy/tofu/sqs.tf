// ===========================================================================
// SQS queues — the declaration
// ===========================================================================
//
// WHY THIS EXISTS ALONGSIDE 10-queues.py
// -------------------------------------
// deploy/localstack/init/10-queues.py provisions the same three queues with
// boto3, and it is NOT replaced by this file. That script is what makes
// `docker compose up` self-sufficient: the LocalStack image mounts its
// directory into /etc/localstack/init/ready.d and runs it on boot, with no
// toolchain on the host. That property is worth keeping.
//
// What it cannot do is the part that does not exist locally. Measured on
// LocalStack 4.14.0 community, and recorded in deploy/localstack/README.md:
//
//   * The queue resource policy (the `Policy` attribute) is STORED BUT NOT
//     ENFORCED. A `Deny * on sqs:*` was written and SendMessage still succeeded.
//   * IAM and STS are `disabled` in _localstack/health. Only sqs is `running`.
//   * KMS is disabled too, so customer-managed keys are not available.
//
// So the resource policy, the per-service IAM split and the CMK — the three
// things that make an SQS deployment actually defensible — can be *written*
// locally but not *proven* locally. Here they are written once, against a
// target that can be either; the local run stays the boto script.
//
// The drift between the two is not left to discipline: see
// test/integration/tofu_sqs_sync_test.go, which reads both and fails when the
// numbers disagree. It is the same treatment the realm and the LocalStack init
// already get (realm_sync_test.go), for the same reason.
//
// IDEMPOTENCE, FOR FREE
// ---------------------
// create_queue is NOT idempotent for an attribute change: it is rejected with
// QueueAlreadyExists and the old attributes stay. That bug was measured, and
// 10-queues.py had to grow a get_queue_url + set_queue_attributes dance to work
// around it. OpenTofu gets this from the state: a second `apply` is a no-op and
// an attribute edit is a single in-place update. Verified against the local
// LocalStack before this file was written:
//
//   apply (2nd time)  -> No changes. Your infrastructure matches the configuration.
//   visibility 60->90 -> 0 to add, 1 to change, 0 to destroy  (server: 90)
//
// WHAT THIS FILE DELIBERATELY DOES NOT CLAIM
// ------------------------------------------
// Pointing this at real AWS is a declaration, not a measurement: no AWS
// credentials were available to this repository, so the only thing proven here
// is that the configuration validates, plans, and applies against LocalStack.
// The policy below is the shape it should have in AWS; treat it as such.

locals {
  prefix = var.name_prefix

  // Queue names. Kept as locals rather than inline strings so the sync test has
  // exactly one place to read them from.
  dlq_name    = "${local.prefix}wager-transactions-dlq.fifo"
  work_name   = "${local.prefix}wager-transactions.fifo"
  events_name = "${local.prefix}integration-events.fifo"
}

// ---------------------------------------------------------------------------
// wager-transactions-dlq.fifo — the quarantine.
//
// Retention is deliberately different from the source queue: a poison message
// is the evidence of a bug, and 4 days is not enough time to notice one.
// ---------------------------------------------------------------------------
resource "aws_sqs_queue" "dlq" {
  name                        = local.dlq_name
  fifo_queue                  = true
  content_based_deduplication = true
  visibility_timeout_seconds  = var.visibility_timeout_seconds
  message_retention_seconds   = var.evidence_retention_seconds
  receive_wait_time_seconds   = var.long_poll_seconds
  sqs_managed_sse_enabled     = var.sse_enabled
  max_message_size            = var.max_message_size_bytes
}

// ---------------------------------------------------------------------------
// wager-transactions.fifo — the work queue the consumer drains.
//
// RedrivePolicy is split out into aws_sqs_queue_redrive_policy because that is
// the provider's current shape; the ARN it needs is the DLQ's, which is why the
// DLQ is declared first.
// ---------------------------------------------------------------------------
resource "aws_sqs_queue" "work" {
  name                        = local.work_name
  fifo_queue                  = true
  content_based_deduplication = true
  visibility_timeout_seconds  = var.visibility_timeout_seconds
  message_retention_seconds   = var.work_retention_seconds
  receive_wait_time_seconds   = var.long_poll_seconds
  sqs_managed_sse_enabled     = var.sse_enabled
  max_message_size            = var.max_message_size_bytes
}

resource "aws_sqs_queue_redrive_policy" "work" {
  queue_url = aws_sqs_queue.work.id

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}

// ---------------------------------------------------------------------------
// integration-events.fifo — the outbox relay's destination.
//
// ContentBasedDeduplication is false on purpose: the publisher supplies an
// explicit MessageDeduplicationId (the event id), so the dedup key is the
// *identity of the event*, never a hash of its body. Two distinct events with
// coincidentally identical bodies must not collapse into one.
// ---------------------------------------------------------------------------
resource "aws_sqs_queue" "events" {
  name                        = local.events_name
  fifo_queue                  = true
  content_based_deduplication = false
  visibility_timeout_seconds  = var.visibility_timeout_seconds
  message_retention_seconds   = var.evidence_retention_seconds
  receive_wait_time_seconds   = var.long_poll_seconds
  sqs_managed_sse_enabled     = var.sse_enabled
  max_message_size            = var.max_message_size_bytes
}

// ---------------------------------------------------------------------------
// Resource policies — the part LocalStack cannot enforce.
//
// These are written here, and asserted as a *shape* by the sync test, but no
// claim is made that they take effect locally: 10-queues.py provisions none,
// for the reason measured above, and
// TestSQSResourcePolicyIsAcceptedButNotEnforced fixes that behaviour in a test
// so it starts failing the day a LocalStack enforces them.
//
// The intent is the standard two-clause shape:
//   * any action without TLS is denied outright (aws:SecureTransport);
//   * the work queue can only be written by the publisher role.
//
// The principal is a placeholder until a per-service IAM split exists — see
// deploy/localstack/README.md. Writing a plausible-looking ARN here would be
// worse than an obvious placeholder, so it stays obviously a placeholder.
// ---------------------------------------------------------------------------
locals {
  // Obvious placeholder: replace with the publisher role ARN when IAM exists.
  publisher_principal = "arn:aws:iam::000000000000:role/REPLACE-with-publisher-role"
}

resource "aws_sqs_queue_policy" "work" {
  queue_url = aws_sqs_queue.work.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyNonTLS"
        Effect    = "Deny"
        Principal = "*"
        Action    = "sqs:*"
        Resource  = aws_sqs_queue.work.arn
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
      {
        Sid       = "AllowPublisherSend"
        Effect    = "Allow"
        Principal = { AWS = local.publisher_principal }
        Action    = "sqs:SendMessage"
        Resource  = aws_sqs_queue.work.arn
      },
    ]
  })
}

resource "aws_sqs_queue_policy" "events" {
  queue_url = aws_sqs_queue.events.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyNonTLS"
        Effect    = "Deny"
        Principal = "*"
        Action    = "sqs:*"
        Resource  = aws_sqs_queue.events.arn
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
      {
        Sid       = "AllowRelaySend"
        Effect    = "Allow"
        Principal = { AWS = local.publisher_principal }
        Action    = "sqs:SendMessage"
        Resource  = aws_sqs_queue.events.arn
      },
    ]
  })
}

// The DLQ accepts sends from the source queue's redrive, not from clients, so
// it gets the TLS clause only.
resource "aws_sqs_queue_policy" "dlq" {
  queue_url = aws_sqs_queue.dlq.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyNonTLS"
        Effect    = "Deny"
        Principal = "*"
        Action    = "sqs:*"
        Resource  = aws_sqs_queue.dlq.arn
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
    ]
  })
}
