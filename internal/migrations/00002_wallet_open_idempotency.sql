-- +goose Up
-- Durable idempotency for POST /wallets.
--
-- Opening a wallet is a financial mutation like any other: it creates the
-- first ledger row (the OPENING transaction) that later movements add to.
-- Replaying it must therefore never open a second wallet or double the
-- opening balance. Uniqueness of (player_id, currency) alone is not enough:
-- it makes a replay fail with a conflict instead of reproducing the original
-- success, and it cannot tell a retry of the SAME payload apart from a
-- genuinely different request for the same player/currency.
--
-- This table gives wallet opening the same guarantee external wagers already
-- have through wager_transactions.idempotency_key + payload_hash:
--   * idempotency_key is the durable identity supplied by the caller;
--   * payload_hash distinguishes "same request replayed" (replay the stored
--     result) from "same key, different body" (409 IDEMPOTENCY_KEY_CONFLICT).
-- The row is written in the SAME transaction as the wallet and its OPENING
-- ledger entry, so the key and the money commit or roll back together.
CREATE TABLE wallet_open_requests (
    idempotency_key text PRIMARY KEY,
    payload_hash    text NOT NULL,
    wallet_id       uuid NOT NULL REFERENCES wallets(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS wallet_open_requests;
