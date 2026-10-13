> This repository's conventions are not style: they encode correctness decisions.
> `Money` as an integer, UUIDv7 identifiers, typed domain errors,
> transactions through `TxManager` — each one exists because the
> alternative costs a bug. This page collects the rules and the reason behind each.

## Hexagonal in practice

The dependency rule is the oldest convention in the project: **`domain` and
`service` import no infrastructure**.

| Layer | May import | May not import |
| :-- | :-- | :-- |
| `domain` | standard library, `time`, `github.com/google/uuid` | `database/sql`, `net/http`, adapters |
| `service` | `domain`, port packages | `postgres`, `sqs`, `transport/http` |
| Adapters | `service` (ports), `domain` | their own business rules |

```go
// Right: the service declares the port; the adapter implements it.
type WalletRepository interface {
    GetForUpdate(ctx context.Context, id domain.WalletID) (*domain.Wallet, error)
}

// Wrong: the service knowing about pgx, the ORM, the table.
```

> [!TIP]
> **The architecture test**
>
> If adding a business rule forced you to import an adapter inside `service`,
> the port is missing. That is a design signal, not an import detail.


## `Money` is a value object in minor units

**Never use `float` for money.** `Money` is an `int64` in minor units (cents)
plus the currency.

```go
// The shape: an integer in minor units + the currency.
type Money struct {
    Amount   int64     // minor units: 1000 == 10.00
    Currency Currency  // ISO 4217, three letters
}
```

| Rule | Why |
| :-- | :-- |
| `int64` in minor units | Floating point does not represent `0.1` exactly; balance sums accumulate error. |
| Currency travels with the value | Adding different currencies is an error, not an implicit conversion. |
| Overflow is checked | `ErrMoneyOverflow` exists because `int64` overflows silently. |
| Operations, not raw arithmetic | `Add`, `Sub`, `Neg` validate before returning. |

> [!WARNING]
> **`float64` is banned at the boundary, not just in the computation**
>
> A single `float64` in a JSON codec or a DTO already contaminates the path:
> `10.00` goes in, `9.999999999999998` comes out, and the balance closes wrong.
> Parse straight into minor units.


## Identifiers are UUIDv7

| Property | Benefit |
| :-- | :-- |
| Time-ordered | Inserts go to the **end** of the B-tree index. |
| Globally unique | No central coordination to generate them. |
| No cross-replica collision | Three replicas generate IDs without talking to each other. |

> [!NOTE]
> **Why not UUIDv4**
>
> UUIDv4 is random, so every `INSERT` lands at a random point in the index —
> and the index suffers constant *page splits*. UUIDv7 gives **index locality**
> and preserves the time ordering a sequential `id` would give, without the
> bottleneck of a central generator.


## Typed domain errors

Every business error is a `*domain.Error` with a **stable `Code`** and a readable
`Message`.

```go
// The code is external contract: HTTP body, failureCode on the transaction.
var ErrInsufficientFunds = &Error{Code: "INSUFFICIENT_FUNDS", Message: "..."}

// Add context without losing identity for errors.Is.
return ErrInsufficientFunds.With("wallet %s has %s", id, bal)
```

| Rule | Why |
| :-- | :-- |
| A `Code` never changes meaning | It is external contract; dashboards and clients depend on it. |
| `With()` preserves identity | `errors.Is(err, ErrSentinel)` still holds along the chain. |
| Infrastructure errors ≠ domain errors | Only domain ones become `422`/`409`; the rest become `5xx`. |

## Transactions through `TxManager`

Every write goes through `TxManager.WithinTx`. Repositories obtained outside a
transaction are **not valid** for writes.

```go
// One transaction, one effect: ledger + inbox + outbox together.
err := tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
    if err := wallets.GetForUpdate(ctx, id); err != nil {
        return err
    }
    if err := ledger.Append(ctx, postings); err != nil {
        return err
    }
    return outbox.Enqueue(ctx, event)   // SAME transaction
})
```

> [!WARNING]
> **A repository outside the transaction is a programming error**
>
> It is not an artificial restriction: row-level locking, optimistic versioning
> and outbox atomicity only exist **inside** a transaction. Using the repository
> outside it discards exactly the guarantee the design pays for.


## Idempotency and keys

The idempotency key is unique per operation and compared together with the body.

| Case | Result |
| :-- | :-- |
| Same key, same body | Idempotent replay — same response, no new effect. |
| Same key, different body | `409 IDEMPOTENCY_KEY_CONFLICT`. |
| New key | Normal processing. |

> [!NOTE]
> **Rejections are persisted too**
>
> A rejection (`422`) records the attempt. Dedup does not only protect the
> happy path — it covers a repeat of the same rejection.


## Comments and commits in Portuguese

- **Code comments**: in Portuguese, explaining *why*, not *what*.
- **Commit messages**: in Portuguese.
- **Documentation**: this site is bilingual (PT/EN), mirrored.

> [!TIP]
> **Comment the decision, not the line**
>
> `// increments the counter` helps nobody. `// the row lock ensures two
> concurrent wagers do not read the same balance` is what saves the next reader.


## Integration tests carry a build tag

Tests that depend on Postgres, SQS or Keycloak live under the `integration` tag
and only run in `make test-integration`.

```go
//go:build integration

package integration
```

| Rule | Why |
| :-- | :-- |
| `integration` tag for stack tests | `make test` stays fast and offline. |
| Sync tests without a tag | They read files; they always run, they cost nothing. |
| Descriptive test names | The name is what shows up in the CI failure. |

## Checklist before opening a PR

| Item | Where |
| :-- | :-- |
| Dependency points inwards | `internal/domain`, `internal/service` |
| Money is never `float` | `internal/domain/money.go` |
| Identifiers are UUIDv7 | `internal/domain/ids.go` |
| Business errors with a stable `Code` | `internal/domain/errors.go` |
| Writes inside `WithinTx` | `internal/postgres/tx_repo.go` |
| New metric documented | `internal/platform/metrics` + dashboard |
| New variable in `config` | `internal/platform/config/config.go` |
| Queues in sync (boto + tofu) | `deploy/localstack/init` + `deploy/tofu` |
| `make test` green | — |

See also: [Technical decisions](decisions.md) ·
[Project structure](project-structure.md) ·
[Testing](../operations/testing.md).
