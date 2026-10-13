variable "region" {
  description = "AWS region the queues live in."
  type        = string
  default     = "us-east-1"
}

variable "sqs_endpoint" {
  description = <<-EOT
    SQS API endpoint. Empty means real AWS; set to http://localhost:4566 to
    target the LocalStack instance from docker-compose.

    This one variable is what makes a single declaration serve two targets.
    Against LocalStack it is a fast local mirror of the same queues; against
    AWS it is the deployment that actually enforces the policies below.
  EOT
  type        = string
  default     = ""
}

variable "name_prefix" {
  description = "Prefix for every queue name."
  type        = string
  default     = ""
}

// ---------------------------------------------------------------------------
// Queue attributes.
//
// These mirror deploy/localstack/init/10-queues.py on purpose, and the mirror
// is enforced by test/integration/tofu_sqs_sync_test.go rather than by
// discipline: the two files are read by that test and compared. Two copies of
// one truth drift — this repository has already paid for that lesson once,
// between docker-compose and the Helm chart.
// ---------------------------------------------------------------------------

variable "work_retention_seconds" {
  description = "Retention of the work queue. 4 days: a longer outage loses the request."
  type        = number
  default     = 345600
}

variable "evidence_retention_seconds" {
  description = "Retention of queues whose messages are evidence: the SQS maximum, 14 days."
  type        = number
  default     = 1209600
}

variable "visibility_timeout_seconds" {
  description = <<-EOT
    Must exceed the consumer's own processing budget. If it does not, a slow but
    correct message is redelivered while still in flight — the classic
    duplicate-processing bug.
  EOT
  type        = number
  default     = 60
}

variable "long_poll_seconds" {
  description = "Long-poll ceiling; AWS caps this at 20."
  type        = number
  default     = 20
}

variable "max_receive_count" {
  description = <<-EOT
    How many deliveries before a poison message is quarantined in the DLQ.
    This is the difference between "the consumer is busy" and "the consumer will
    never succeed".
  EOT
  type        = number
  default     = 5
}

variable "max_message_size_bytes" {
  description = <<-EOT
    Maximum message size. MUST stay at the AWS/SQS default of 1 MiB.

    This variable exists because the provider's own default is WRONG for this
    service: hashicorp/aws provisions 262144 (256 KiB) when the attribute is
    omitted, while SQS itself — and the boto script — use 1048576 (1 MiB).
    Measured: a queue created with the attribute omitted came back as 262144.

    That is not cosmetic here. internal/transport/http/codecs.go reads request
    bodies up to 1<<20, so a request the API accepts can be up to 1 MiB; at
    256 KiB the publisher would fail on messages the service had already
    validated. The value is therefore pinned explicitly rather than inherited.
  EOT
  type        = number
  default     = 1048576
}

variable "sse_enabled" {
  description = "At-rest encryption with an AWS-managed key (sqs_managed_sse_enabled)."
  type        = bool
  default     = true
}
