output "dlq_url" {
  description = "URL of the dead-letter queue."
  value       = aws_sqs_queue.dlq.id
}

output "work_url" {
  description = "URL of the work queue (WALLET_SQS_REQUEST_QUEUE's target)."
  value       = aws_sqs_queue.work.id
}

output "events_url" {
  description = "URL of the outbox relay's destination (WALLET_SQS_EVENTS_QUEUE's target)."
  value       = aws_sqs_queue.events.id
}

output "queue_names" {
  description = "The three queue names, as the service expects them under a .fifo suffix check."
  value = {
    dlq    = aws_sqs_queue.dlq.name
    work   = aws_sqs_queue.work.name
    events = aws_sqs_queue.events.name
  }
}
