-- +goose Up
-- Double-entry postings: the second leg of every wallet movement.
--
-- Why this exists next to wallet_ledger_entries rather than replacing it.
--
-- The existing ledger is SINGLE-SIDED. One row per movement records what
-- happened to ONE account (the player's wallet): "the balance went from X to
-- X - amount". Audit #4 proves the stored balance equals the sum of those rows,
-- so each individual account is internally consistent. What that shape can
-- NEVER express is the property that makes a ledger a ledger: that a debit
-- somewhere is a credit somewhere else, so value is conserved by the system as
-- a whole. A bug that debits "the void" instead of the house passes audit #4
-- untouched — the player's balance is right, the money simply appears or
-- disappears from nowhere.
--
-- The fix is not to rewrite the ledger (the append-only chain, the CHECK
-- arithmetic and every existing audit keep working on it unchanged). It is to
-- ADD the missing leg in a second table. `internal/domain/doubleentry.go`
-- already models this — Account / Posting / JournalEntry with a Balanced()
-- invariant — and until now it had no call site: the model existed, the SQL it
-- would write did not. This migration is the storage half.
--
-- Invariant, and it is a property of a TRANSACTION, not of a row:
--
--   SELECT transaction_id FROM postings
--   GROUP BY transaction_id
--   HAVING SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END) <> 0;
--
-- must return nothing: every journal entry balances. Audit #11 in
-- loadtest/sql/invariants.sql asserts exactly this query returns 0 rows.
--
-- Relationship to wallet_ledger_entries: for a movement on a player's wallet,
-- the player's posting here has the SAME (transaction_id, direction, amount) as
-- the ledger row. The postings table is a superset — it also carries the
-- counterparty leg — so the ledger stays the fast path for balance math and the
-- journal becomes the audit path that can answer "and who was on the other
-- side?". A trigger below keeps the two from ever disagreeing on the player
-- leg: the player posting is the ledger row, or the transaction fails.
CREATE TABLE postings (
    id             uuid PRIMARY KEY,
    transaction_id uuid NOT NULL REFERENCES wager_transactions(id),
    -- Chart of accounts identity: (account_kind, account_ref). account_ref is
    -- the wallet id for PLAYER_WALLET, a provider id for PROVIDER_SETTLEMENT,
    -- and '' for the singletons (HOUSE, REVENUE). The domain enforces which
    -- kinds carry a ref; the CHECK below re-imposes the shape at the DB level.
    account_kind   text NOT NULL CHECK (account_kind IN
        ('PLAYER_WALLET','HOUSE','PROVIDER_SETTLEMENT','REVENUE')),
    account_ref    text NOT NULL DEFAULT '',
    direction      text NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    currency       char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- A posting that moves nothing is not a posting: the domain rejects zero
    -- legs and the DB agrees.
    amount_minor   bigint NOT NULL CHECK (amount_minor > 0),
    created_at     timestamptz NOT NULL DEFAULT now(),

    -- Reference shape: a player wallet names a wallet, a settlement account
    -- names a provider, the singletons name nobody.
    CONSTRAINT postings_ref_shape CHECK (
        (account_kind = 'PLAYER_WALLET' AND account_ref <> '')
        OR (account_kind = 'PROVIDER_SETTLEMENT' AND account_ref <> '')
        OR (account_kind IN ('HOUSE','REVENUE') AND account_ref = '')
    )
);

-- One account may appear at most once per direction in a transaction. Two
-- credit legs against the same account (as EntryForMovement never produces, but
-- a future multi-leg entry might) would make the per-account aggregate
-- ambiguous; forbidding it keeps AccountBalance well-defined.
CREATE UNIQUE INDEX postings_tx_account_dir_uq
    ON postings (transaction_id, account_kind, account_ref, direction);

-- The balance query groups by transaction (audit #11); this index makes it a
-- per-transaction lookup rather than a scan.
CREATE INDEX postings_tx_idx ON postings (transaction_id);

-- Postings are append-only for the same reason the ledger is: a journal that
-- can be edited after the fact is not evidence. Correct a mistake with a
-- reversing entry, never with an UPDATE.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION forbid_posting_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'postings is append-only (attempted %)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER posting_no_update BEFORE UPDATE ON postings
    FOR EACH ROW EXECUTE FUNCTION forbid_posting_mutation();
CREATE TRIGGER posting_no_delete BEFORE DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION forbid_posting_mutation();

-- The player leg cannot contradict the ledger. When a player posting is
-- inserted, the corresponding ledger row for the SAME transaction must already
-- exist with the same wallet, direction and amount. This is
-- unconditional — every path that writes a player posting (OPENING on wallet
-- creation, BET/WIN, refunds and rollbacks) writes its ledger row in the same
-- SQL transaction first, so the row is always there to be found. What the
-- check forbids is the seductive bug of writing the journal "on the side",
-- diverging from the money that actually moved.
--
-- The check runs BEFORE INSERT because that is the direction the application
-- writes: ledger first, then postings. The mirror case — a PROCESSED movement
-- with no player posting at all — is a missing-row bug that a row trigger
-- cannot see, and is caught by audit #11b in loadtest/sql/invariants.sql.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION posting_matches_ledger() RETURNS trigger AS $$
BEGIN
    IF NEW.account_kind = 'PLAYER_WALLET' THEN
        IF NOT EXISTS (
            SELECT 1 FROM wallet_ledger_entries l
            WHERE l.transaction_id = NEW.transaction_id
              AND l.wallet_id::text = NEW.account_ref
              AND l.direction = NEW.direction
              AND l.amount_minor = NEW.amount_minor
        ) THEN
            RAISE EXCEPTION
                'player posting for tx % has no matching ledger row (wallet %, % %)',
                NEW.transaction_id, NEW.account_ref, NEW.direction, NEW.amount_minor
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER posting_matches_ledger_check BEFORE INSERT ON postings
    FOR EACH ROW EXECUTE FUNCTION posting_matches_ledger();

-- +goose Down
DROP TRIGGER IF EXISTS posting_matches_ledger_check ON postings;
DROP FUNCTION IF EXISTS posting_matches_ledger();
DROP TRIGGER IF EXISTS posting_no_update ON postings;
DROP TRIGGER IF EXISTS posting_no_delete ON postings;
DROP FUNCTION IF EXISTS forbid_posting_mutation();
DROP TABLE IF EXISTS postings;
