-- +goose Up
-- Cross-process trace continuity for the transactional outbox.
--
-- The outbox breaks traces in two by construction: the HTTP request commits a
-- row and returns, while a detached relay goroutine later republishes that row
-- to SQS from a context born of context.Background(). Nothing in that second
-- context points back to the request that caused the event, so the trace the
-- provider sees (relay → SQS → consumer) is a stranger to the trace the
-- provider started (HTTP submit). The relay cannot replay what it never saw.
--
-- Persisting the W3C traceparent on the row at enqueue time is what bridges
-- the two halves: the value is written inside the SAME transaction as the
-- balance change (so it commits or rolls back with the money), the relay
-- reads it back as data, and the consumer resumes the trace as a child.
--
-- Stored as text, NOT NULL DEFAULT '' — the same shape as correlation_id
-- beside it: an empty value means "no upstream context" (a bare API call
-- without a traceparent) and is treated as "start a new trace", never as an
-- error. Additive migration: existing rows keep publishing; they simply carry
-- no link, exactly as they do today.
ALTER TABLE integration_outbox ADD COLUMN trace_parent text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE integration_outbox DROP COLUMN trace_parent;
