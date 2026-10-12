-- +goose Up
-- Schema for the wallet service. All financial invariants are imposed here,
-- independently of application locks or SQS deduplication.

CREATE TABLE wallets (
    id             uuid PRIMARY KEY,
    player_id      uuid NOT NULL,
    currency       char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor  bigint NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    version        bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT wallets_player_currency_uq UNIQUE (player_id, currency)
);

CREATE TABLE wager_transactions (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
    origin text NOT NULL CHECK (origin IN ('INTERNAL','EXTERNAL')),
    provider_id text,
    external_transaction_id text,
    idempotency_key text,
    payload_hash text,
    player_id uuid NOT NULL,
    wallet_id uuid NOT NULL REFERENCES wallets(id),
    round_id text,
    game_id text,
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_minor bigint NOT NULL CHECK (amount_minor >= 0),
    reference_external_transaction_id text,
    reference_transaction_id uuid REFERENCES wager_transactions(id),
    status text NOT NULL CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
    failure_code text,
    failure_message text,
    result_direction text CHECK (result_direction IN ('DEBIT','CREDIT')),
    result_amount_minor bigint NOT NULL DEFAULT 0 CHECK (result_amount_minor >= 0),
    balance_before_minor bigint,
    balance_after_minor bigint,
    wallet_version_after bigint,
    channel text NOT NULL CHECK (channel IN ('HTTP','SQS')),
    correlation_id text NOT NULL DEFAULT '',
    pending_attempts int NOT NULL DEFAULT 0,
    next_retry_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- origin/shape coherence: external rows carry the full external identity,
    -- internal (OPENING) rows carry none of it.
    CONSTRAINT tx_external_shape CHECK (
        (origin = 'EXTERNAL' AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
        OR
        (origin = 'INTERNAL' AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL AND kind = 'OPENING')
    ),
    -- reversals must reference an external transaction
    CONSTRAINT tx_reversal_reference CHECK (
        kind NOT IN ('REFUND','ROLLBACK') OR reference_external_transaction_id IS NOT NULL
    ),
    -- LOSS is informational and carries zero
    CONSTRAINT tx_loss_zero CHECK (kind <> 'LOSS' OR amount_minor = 0),
    -- financial operations require a positive amount
    CONSTRAINT tx_positive_amount CHECK (kind IN ('LOSS','OPENING') OR amount_minor > 0),
    -- terminal coherence
    CONSTRAINT tx_processed_clean CHECK (status <> 'PROCESSED' OR failure_code IS NULL),
    CONSTRAINT tx_rejected_has_code CHECK (status <> 'REJECTED' OR failure_code IS NOT NULL),
    CONSTRAINT tx_failed_has_code CHECK (status <> 'FAILED' OR failure_code IS NOT NULL)
);

-- Durable idempotency identity (survives every restart; DB-level, so HTTP and
-- SQS dedup against the same rows).
CREATE UNIQUE INDEX tx_provider_key_uq ON wager_transactions (provider_id, idempotency_key);
CREATE UNIQUE INDEX tx_provider_external_uq ON wager_transactions (provider_id, external_transaction_id);

-- One OPENING per wallet: prevents duplicated initial credit even under
-- concurrent openings.
CREATE UNIQUE INDEX tx_opening_per_wallet_uq ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- At most one SUCCESSFUL reversal of each kind per referenced transaction:
-- a BET cannot be refunded twice, a WIN cannot be rolled back twice, etc.
CREATE UNIQUE INDEX tx_refund_per_ref_uq ON wager_transactions (reference_transaction_id)
    WHERE kind = 'REFUND' AND status = 'PROCESSED' AND reference_transaction_id IS NOT NULL;
CREATE UNIQUE INDEX tx_rollback_per_ref_uq ON wager_transactions (reference_transaction_id)
    WHERE kind = 'ROLLBACK' AND status = 'PROCESSED' AND reference_transaction_id IS NOT NULL;

-- Worker scan indexes
CREATE INDEX tx_pending_ref_idx ON wager_transactions (next_retry_at) WHERE status = 'PENDING_REFERENCE';
CREATE INDEX tx_wallet_idx ON wager_transactions (wallet_id, created_at);
CREATE INDEX tx_provider_pending_idx ON wager_transactions (provider_id, status);

CREATE TABLE wallet_ledger_entries (
    id uuid PRIMARY KEY,
    wallet_id uuid NOT NULL REFERENCES wallets(id),
    transaction_id uuid NOT NULL REFERENCES wager_transactions(id),
    direction text NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    balance_before_minor bigint NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor bigint NOT NULL CHECK (balance_after_minor >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- (wallet, transaction) moves money at most once
    CONSTRAINT ledger_wallet_tx_uq UNIQUE (wallet_id, transaction_id),
    -- arithmetic + non-negativity re-imposed at the bank level
    CONSTRAINT ledger_arithmetic CHECK (
        (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
        OR (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
    )
);
CREATE INDEX ledger_wallet_created_idx ON wallet_ledger_entries (wallet_id, created_at, id);

-- Append-only enforcement: UPDATE and DELETE raise, they never succeed.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forbid_ledger_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (attempted %)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_no_update BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();
CREATE TRIGGER ledger_no_delete BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

-- Inbox: durable at-least-once consumption identity per consumer.
CREATE TABLE consumer_inbox (
    consumer_name text NOT NULL,
    message_id text NOT NULL,
    message_hash text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (consumer_name, message_id)
);

-- Transactional outbox: rows committed with the domain data they describe.
CREATE TABLE integration_outbox (
    event_id uuid PRIMARY KEY,
    aggregate_type text NOT NULL,
    aggregate_id uuid NOT NULL,
    event_type text NOT NULL,
    event_version int NOT NULL,
    correlation_id text NOT NULL DEFAULT '',
    payload jsonb NOT NULL,
    attempts int NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbox_pending_idx ON integration_outbox (available_at) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS integration_outbox;
DROP TABLE IF EXISTS consumer_inbox;
DROP TRIGGER IF EXISTS ledger_no_update ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS ledger_no_delete ON wallet_ledger_entries;
DROP FUNCTION IF EXISTS forbid_ledger_mutation();
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;
