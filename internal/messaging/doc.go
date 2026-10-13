// Package messaging hosts the background workers of the wallet service:
// the SQS inbox consumer, the outbox relay, the pending-reference resolver
// and the continuous reconciliation sweeper. All of them:
//   - stop accepting new work on ctx cancellation (SIGTERM),
//   - finish or release in-flight work within the shutdown grace period,
//   - surface progress through the Instrumentation/ConsumerMetrics ports.
package messaging
