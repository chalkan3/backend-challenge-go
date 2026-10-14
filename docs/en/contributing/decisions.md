> Every non-obvious decision in this project has a reason that can be measured.
> This page is the record of *why*: what was chosen, what was rejected and
> which failure mode the choice avoids. It is not a diary — it is a contract with
> whoever touches the code next.

> [!TIP]
> **How to read this page**
>
> Every section follows the same shape: **Decision**, **Rejected alternative**,
> **Why**. If you are about to reverse one of them, read the failure mode
> first.


## UUIDv7 instead of UUIDv4 (or sequential)

**Decision:** every identifier is UUIDv7.

**Rejected:** UUIDv4 (random) and `bigserial` (central generator).

**Why:** UUIDv7 is time-ordered. Inserts go to the **end** of the B-tree index —
without the constant *page splits* that are v4's classic problem. And unlike a
`bigserial`, it requires no central coordination: three replicas generate IDs
without talking to each other.

| Property | Result |
| :-- | :-- |
| Time-ordered | Index locality. |
| Locally generated | No sequence bottleneck. |
| Globally unique | No cross-replica collision. |

## Transactional outbox instead of direct publish (and 2PC)

**Decision:** the event is written in the **same transaction** as the state
change and published afterwards, asynchronously.

**Rejected:** publishing straight to SQS inside the use case; and 2PC
(two-phase commit) between Postgres and SQS.

**Why:** publishing after the commit leaves a window — the process dies between
the commit and the `SendMessage`, and the event is lost. Publishing before the
commit lets the event out while the transaction may still roll back, and then the
world receives a fact that never happened. 2PC requires both participants to
support the protocol — SQS does not. The outbox removes the window: if the
transaction committed, the event exists; if it rolled back, the event does not.

> [!NOTE]
> **What the outbox costs**
>
> Latency (the event leaves on the next tick, not on the same breath) and a
> publisher with a *lease*. In exchange, the effect and the event are
> **atomic**.


## Durable inbox instead of SQS-only dedup

**Decision:** an inbox table records the processed key **in the same transaction**
as the effect.

**Rejected:** relying on SQS FIFO and `MessageDeduplicationId`.

**Why:** SQS guarantees *at-least-once* delivery; the FIFO deduplication window
is **5 minutes**. A message redelivered **after** that window — following a long
visibility timeout, a retry, or a manual reprocess — would pass as new. The
durable inbox deduplicates by **storage lifetime**, which is the only thing that
covers the real window of an incident.

> [!WARNING]
> **The inbox does not replace FIFO**
>
> SQS still does its part (ordering per group, redrive to the DLQ). The inbox
> is the **second** layer, because the first one has a time ceiling.


## Row-level lock + optimistic version, not global serializable

**Decision:** the wallet is locked (`SELECT ... FOR UPDATE`) and the row carries a
`version`; the update is optimistic against that version.

**Rejected:** global `SERIALIZABLE` isolation.

**Why:** global `SERIALIZABLE` makes **every** transaction conflict with the
others, even those that do not intersect. With wagers on different wallets —
which is the common case — the cost is cascading retries with no correctness gain.
Row-level locking serialises only what actually competes: **two wagers on the same
wallet**. The rest proceed in parallel.

```sql
-- The conflict is localised on the wallet row, not on the whole database.
SELECT balance, version FROM wallets WHERE id = $1 FOR UPDATE;
UPDATE wallets SET balance = $2, version = version + 1 WHERE id = $1 AND version = $3;
```

> [!NOTE]
> **That is why `hot-wallet` exists in the load test**
>
> The `hot-wallet` scenario points N VUs at **one** wallet. It is the empirical
> proof that the lock serialises without lost updates — and that the rest of the
> traffic stays parallel.


## Money in `int64` minor units, never `float`

**Decision:** monetary values are integers in minor units, plus the currency.

**Rejected:** `float64`, a library `decimal`, string arithmetic.

**Why:** `float64` does not represent `0.1` exactly; balance sums accumulate
error and a ledger that does not close cannot be fixed afterwards. Minor units in
`int64` are exact by construction, and the currency travels with the value so that
adding different currencies is an **error**, not a silent conversion.

## Ports & adapters

**Decision:** hexagonal — the dependency points inwards.

**Rejected:** layer-based organisation with the domain depending on the ORM or a
concrete repository.

**Why:** the domain can only be tested without infrastructure if it **does not
know** infrastructure. 399 top-level tests (778 subtests) run without a stack because of that. And
swapping the infrastructure (LocalStack for AWS, Postgres for another store) does
not touch the business rules.

## One binary with three modes

**Decision:** `cmd/walletd` starts as `all`, `api` or `worker`.

**Rejected:** three binaries, or only `all`.

**Why:** three binaries would duplicate the graph assembly and diverge over time.
Only `all` prevents separating API load from worker load. One binary with a mode
gives both: the **same** image, the **same** graph, with roles chosen by a
variable.

| Mode | What for |
| :-- | :-- |
| `all` | Development and the compose environment. |
| `api` | Scale HTTP without scaling consumers. |
| `worker` | Scale consumers without multiplying API replicas. |

## Optional tofu alongside boto

**Decision:** the queues have two declarations — the boto3 script that provisions
LocalStack and an OpenTofu module — plus a **sync test** between them.

**Rejected:** replacing boto with tofu (it would require a toolchain on the host
and break `make up`).

**Why:** `make up` is self-sufficient today, and that property is worth more than
having a single source of truth. Tofu exists to prove the declaration holds
outside LocalStack — where, measured, the *resource policies* are not even
enforced. The sync test closes the only real risk of the duplication (the two
halves quietly diverging).

> [!WARNING]
> **The trap is documented, not hidden**
>
> Tofu matches resources by **identity** in the state. Applying against an
> already-populated LocalStack collides with `QueueAlreadyExists`. The way out is
> `PREFIX=` for a parallel probe or `tofu import`. See
> [Infrastructure as code](../operations/infrastructure.md).


## Load verification in a separate target

**Decision:** `make load-verify` (SQL invariants) is a target **separate** from
`make load` (k6).

**Rejected:** bundling the verification into the k6 scenarios.

**Why:** k6 is an HTTP client — it **cannot see Postgres**. The accounting
invariants (balance equal to the ledger sum, no half-applied effects) only exist
in the SQL layer. Forcing the verification into k6 would give half a proof: the
script could measure that the API answered `202` without ever knowing whether the
money went to the right place.

| Instrument | What it proves |
| :-- | :-- |
| k6 | It held up under load and answered. |
| `invariants.sql` | What got written is correct. |

See also: [Conventions](conventions.md) ·
[Invariants](../architecture/invariants.md) ·
[Load testing](../operations/load-testing.md) ·
[Infrastructure as code](../operations/infrastructure.md).
