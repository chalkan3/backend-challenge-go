-- Independent integrity audit of the wallet database.
--
-- Why this exists next to the k6 scenarios: k6 sees the API, not the database.
-- It can prove that the service ANSWERED correctly; it cannot prove that what
-- was committed is internally consistent. These queries are the proof, run
-- directly against Postgres with no service in the middle.
--
-- Contract with scripts/load-verify.sh: each audit prints a machine-readable
-- line of the form
--     AUDIT|<n>|<description>|<value>
-- where <value> MUST be 0 for a pass. Everything else in this file is for a
-- human reading the file directly.
--
-- Usage:
--   docker exec -i wallet_postgres_1 psql -U wallet -d wallet -f - < loadtest/sql/invariants.sql

\set ON_ERROR_STOP on
\pset tuples_only on
\pset format unaligned

\echo 'AUDIT note: append-only ledger, no entry may rewrite history'

-- 1. The ledger is a chain: each entry's balance_before is the previous entry's
--    balance_after. A broken chain means history was rewritten.
WITH ordered AS (
    SELECT
        wallet_id,
        balance_before_minor,
        LAG(balance_after_minor) OVER (PARTITION BY wallet_id ORDER BY created_at, id) AS prev_after
    FROM wallet_ledger_entries
)
SELECT 'AUDIT|1|ledger chain intact|' || count(*) FROM ordered
WHERE prev_after IS NOT NULL AND balance_before_minor <> prev_after;

-- 2. Every entry moves exactly its amount in the direction it claims.
SELECT 'AUDIT|2|entry amounts match direction|' || count(*)
FROM wallet_ledger_entries
WHERE (direction = 'CREDIT' AND balance_after_minor <> balance_before_minor + amount_minor)
   OR (direction = 'DEBIT'  AND balance_after_minor <> balance_before_minor - amount_minor);

-- 3. The CHECK constraint should make this impossible; assert it anyway.
SELECT 'AUDIT|3|no negative wallet balance|' || count(*) FROM wallets WHERE balance_minor < 0;

-- 4. The stored balance equals the sum of the wallet's ledger entries.
WITH ledger_sum AS (
    SELECT wallet_id, SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor ELSE -amount_minor END) AS net
    FROM wallet_ledger_entries
    GROUP BY wallet_id
)
SELECT 'AUDIT|4|stored balance equals ledger sum|' || count(*)
FROM wallets w
LEFT JOIN ledger_sum l ON l.wallet_id = w.id
WHERE w.balance_minor <> COALESCE(l.net, 0);

-- 5. version counts the ledger entries (see internal/service/wallet.go: a
--    positive opening yields one entry and version 1).
WITH counts AS (
    SELECT wallet_id, count(*) AS entries FROM wallet_ledger_entries GROUP BY wallet_id
)
SELECT 'AUDIT|5|wallet version is a faithful operation count|' || count(*)
FROM wallets w
LEFT JOIN counts c ON c.wallet_id = w.id
WHERE w.version <> GREATEST(COALESCE(c.entries, 0), 1);

-- 6. One wallet per (player, currency).
SELECT 'AUDIT|6|no duplicate wallets|' || count(*) FROM (
    SELECT player_id, currency FROM wallets GROUP BY player_id, currency HAVING count(*) > 1
) d;

-- 7. One transaction per idempotency identity: this is what makes a replay a
--    replay instead of a second debit.
SELECT 'AUDIT|7|idempotency keys are unique|' || count(*) FROM (
    SELECT provider_id, idempotency_key FROM wager_transactions
    WHERE idempotency_key IS NOT NULL
    GROUP BY provider_id, idempotency_key HAVING count(*) > 1
) d;

-- 8. No PROCESSED money movement without a ledger row behind it.
--    A transaction whose amount is zero moved nothing, so it is not a movement
--    and has no row to find: a fee-less LOSS is the case that exists today.
--    OPENING is deliberately NOT excluded — it writes its own ledger row (the
--    initial credit), and exempting it would hide a real opening that failed to
--    record its balance. Parallelism is disabled for this statement: the
--    Postgres container's shared memory segment is too small for a parallel
--    worker here.
SET max_parallel_workers_per_gather = 0;
SELECT 'AUDIT|8|every processed transaction has a ledger entry|' || count(*)
FROM wager_transactions t
WHERE t.status = 'PROCESSED'
  AND t.amount_minor <> 0
  AND NOT EXISTS (SELECT 1 FROM wallet_ledger_entries l WHERE l.transaction_id = t.id);
RESET max_parallel_workers_per_gather;

-- 9. A REJECTED transaction must not have moved money.
SELECT 'AUDIT|9|rejected transactions moved no money|' || count(*)
FROM wager_transactions t
WHERE t.status = 'REJECTED'
  AND EXISTS (SELECT 1 FROM wallet_ledger_entries l WHERE l.transaction_id = t.id);

-- 10. Informational: the outbox backlog. Not a violation — it is the expected
--     shape when producers outrun the relay — but a large, growing number here
--     is the signal that the relay needs scaling.
SELECT 'INFO|10|outbox pending|' || count(*) FROM integration_outbox WHERE published_at IS NULL;

-- 11. THE double-entry invariant: every journal entry balances. Grouping by
--     transaction and summing credits minus debits, any non-zero total is a
--     transaction that created or destroyed value. This is the check that the
--     single-sided ledger structurally cannot express — it reconciles each
--     account against itself, while this reconciles the accounts against each
--     other.
SELECT 'AUDIT|11|every journal entry balances|' || count(*) FROM (
    SELECT transaction_id
    FROM postings
    GROUP BY transaction_id
    HAVING SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END) <> 0
) d;

-- 11b. Every movement has a journal entry. 11 says entries balance; this says
--      they exist. A bank of accounts that never disagree is also what a bank
--      of NO accounts looks like, so the pair is what actually constrains the
--      system. OPENING is included on purpose: an opening is a credit to the
--      player that has to come from somewhere. A zero-amount transaction is not
--      a movement and posts nothing (a fee-less LOSS), matching audit #8.
SET max_parallel_workers_per_gather = 0;
SELECT 'AUDIT|11b|every processed movement has a journal entry|' || count(*)
FROM wager_transactions t
WHERE t.status = 'PROCESSED'
  AND t.amount_minor <> 0
  AND EXISTS (SELECT 1 FROM wallet_ledger_entries l WHERE l.transaction_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM postings p WHERE p.transaction_id = t.id);
RESET max_parallel_workers_per_gather;

-- 11c. The player leg equals the ledger row. The trigger enforces this on
--      write; this reports whether the historical rows agree, which matters
--      after a migration or a restore that bypassed the trigger.
SELECT 'AUDIT|11c|player postings match their ledger rows|' || count(*)
FROM wallet_ledger_entries l
LEFT JOIN postings p
    ON p.transaction_id = l.transaction_id
    AND p.account_kind = 'PLAYER_WALLET'
    AND p.account_ref = l.wallet_id::text
WHERE p.id IS NULL
   OR p.direction <> l.direction
   OR p.amount_minor <> l.amount_minor;
