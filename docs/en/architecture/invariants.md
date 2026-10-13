> A list of what the system **does not allow**, and in which layer
> each prohibition is enforced. The central idea: financial invariants are
> enforced **in the database**, independently of application locks
> and SQS deduplication. If the application code has a bug, the constraint
> refuses — it does not fix, round, or carry on.

## The invariants, by layer

| # | Invariant | Where it is enforced | What a violation would mean |
| :-: | :-- | :-- | :-- |
| 1 | `balance >= 0` at all times | `CHECK balance_minor >= 0` + domain | money created out of thin air |
| 2 | Balance = sum of the ledger | reconciliation + `loadtest/sql/invariants.sql` | inconsistent history |
| 3 | Ledger rows never change | triggers `ledger_no_update` / `ledger_no_delete` | audit trail destroyed |
| 4 | Ledger arithmetic holds | `CHECK ledger_arithmetic` | amount disagrees with direction |
| 5 | One entry per (wallet, transaction) | `UNIQUE ledger_wallet_tx_uq` | money moved twice |
| 6 | Idempotency key is unique | `UNIQUE tx_provider_key_uq` | a retry applies again |
| 7 | One `OPENING` per wallet | `UNIQUE tx_opening_per_wallet_uq` | duplicated initial credit |
| 8 | One reversal per reference | `UNIQUE tx_refund_per_ref_uq`, `tx_rollback_per_ref_uq` | duplicated refund |
| 9 | `LOSS` carries zero | `CHECK tx_loss_zero` | informational op moving money |
| 10 | Reversal requires a reference | `CHECK tx_reversal_reference` + domain | reversal with no origin |
| 11 | Terminals never reopen | `allowedTransitions` + `canTransition` | an approved op reprocessed |
| 12 | `version` tracks the balance | `CHECK version >= 1` + domain | optimistic concurrency broken |

## The ledger is genuinely append-only

It is not a code convention: it is a database trigger. Any `UPDATE` or `DELETE`
raises, no matter who attempts it.

```sql
CREATE OR REPLACE FUNCTION forbid_ledger_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (attempted %)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_no_update BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();
CREATE TRIGGER ledger_no_delete BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();
```

```mermaid
flowchart LR
    A["UPDATE wallet_ledger_entries"] --> T{"trigger  
ledger_no_update"}
    D["DELETE FROM wallet_ledger_entries"] --> T2{"trigger  
ledger_no_delete"}
    T -->|RAISE EXCEPTION| X["integrity_constraint_violation"]
    T2 -->|RAISE EXCEPTION| X
    I["INSERT"] --> OK["allowed"]
    S["SELECT"] --> OK

    classDef bad fill:#fdecea,stroke:#c0392b
    classDef ok fill:#e6f4ec,stroke:#1a7f4b
    class X bad
    class OK ok
```

## Arithmetic is checked per row

Every entry states the balance before and after. The constraint requires the
maths to hold — the amount *must* agree with the direction:

```sql
CONSTRAINT ledger_arithmetic CHECK (
    (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    OR
    (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
)
```

This makes the ledger **self-verifying**: a `CREDIT` that decreased the balance
would be rejected on insert, even if the domain made the mistake.

## The ledger chain

```mermaid
flowchart TB
    O["OPENING  
after = initial balance"] --> B1["BET  
before = 1000  
after = 900"]
    B1 --> W1["WIN  
before = 900  
after = 1100"]
    W1 --> L1["LOSS  
amount = 0  
(no entry produced)"]
    L1 --> R1["REFUND of the BET  
before = 1100  
after = 1200"]

    classDef money fill:#f4ecfb,stroke:#6b21a8
    class O,B1,W1,R1 money
```

The `invariants.sql` audit walks that chain and fails if any `balance_before`
differs from the previous entry's `balance_after` — which would detect a
rewritten history.

> [!WARNING]
> **What the audit covers that k6 does not**
>
> k6 cannot see Postgres. It proves the API *responds* correctly under
> concurrency; the accounting invariants only exist in the SQL layer. That is
> why `make load-verify` is a separate target: **load without an invariant
> proves nothing**.


## Double-entry accounting

Beyond the per-wallet ledger, the service keeps a **double-entry journal**:
every movement has a counter-entry, and the journal cannot close unbalanced.

| Code | Meaning |
| :-- | :-- |
| `INVALID_ACCOUNT` | account is not in the chart of accounts |
| `UNBALANCED_JOURNAL` | debits ≠ credits |

This is a bonus over the challenge statement — and it is optional: the service
works in full with the simple wallet ledger. The journal is an extra audit
layer, proven in `internal/domain/doubleentry_test.go`.

## Where rejection lives

A business rejection **is not a system error**: it is persisted, with a stable
code, and returned as `422`.

```mermaid
flowchart TD
    IN["operation received"] --> V{"validation  
(400, not persisted)"}
    V -->|invalid| E400["VALIDATION_FAILED"]
    V -->|valid| BIZ{"business rule"}
    BIZ -->|refuses| REJ["status = REJECTED  
failureCode set  
422"]
    BIZ -->|approves| OK["status = PROCESSED  
200"]
    BIZ -->|future dependency| PEND["status = PENDING_REFERENCE  
202"]

    classDef bad fill:#fdecea,stroke:#c0392b
    classDef ok fill:#e6f4ec,stroke:#1a7f4b
    classDef warn fill:#fdf3e0,stroke:#a06a00
    class E400,REJ bad
    class OK ok
    class PEND warn
```

The distinction matters: `400` means **nothing was written**; `422` means the
refusal is a **recorded fact** and auditable, with `failureCode` and
`failureMessage` persisted alongside the operation.

See also: [Domain model](domain-model.md) ·
[Schema](../data/schema.md) · [The ledger](../data/ledger.md).
