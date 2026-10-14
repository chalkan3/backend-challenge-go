> The test suite exists in **four layers**, each with a different
> purpose and prerequisite. This page explains what each target does, what it
> covers and — most importantly — *what it proves that the others do not*.

## The four layers

| Target | Prerequisite | What runs |
| :-- | :-- | :-- |
| `make test` | none | `go test ./...` — fast suite, no stack. |
| `make test-race` | none | Same, with `-race`. Detects data races. |
| `make test-integration` | stack up | Tests with the `integration` build tag against real Postgres, LocalStack and Keycloak. |
| `make e2e` | none (brings up the stack) | `scripts/e2e.sh`: brings up, waits for healthchecks, runs, tears down. |

```sh
# Day to day: fast, no infrastructure.
make test

# Before closing a concurrency change.
make test-race

# With `make up` already running: proves against the real services.
make test-integration

# End-to-end proof, self-contained: brings everything up and tears it down.
make e2e
```

> [!TIP]
> **Run `make test` always; `make test-integration` when the commit touches concurrency**
>
> `make test` is the daily safety net (dozens of unit tests, including the ones
> that read the infrastructure files). `make test-integration` is more
> expensive and slower, but it is the only one that sees Postgres, SQS and
> Keycloak for real.


## The fast suite

- **399 top-level tests** (778 subtests including the table-driven ones) spread across domain, service, adapters and platform,
  according to the README.
- Runs **without a stack**: no Postgres, no queue, no Keycloak.
- Includes the **infrastructure sync** test
  (`test/integration/tofu_sqs_sync_test.go`, no build tag) which reads both
  declarative files and fails if they diverge — without needing the tofu binary.

> [!NOTE]
> **What `make test` deliberately does not do**
>
> It does not start containers and does not depend on the network. That is what
> keeps it fast enough to run on every save. *Concurrency* correctness only
> shows up under `-race` and against a real Postgres — hence
> `make test-race` and `make test-integration`.


## `-race`

```sh
make test-race
```

The race detector finds unsynchronised concurrent accesses. The design uses
row-level locking plus optimistic versioning, and that is exactly the kind of
code `-race` audits. Run it whenever a change touches goroutines, workers or the
`TxManager`.

## Integration tests

They require the stack to be up (`make up`) and are selected by the `integration`
build tag.

```sh
# `integration` build tag; endpoints come from the WALLET_TEST_* environment.
go test -tags=integration ./test/integration/...
```

Variables used by the integration tests:

| Variable | Default value |
| :-- | :-- |
| `WALLET_TEST_API_URL` | `http://localhost:8081` |
| `WALLET_TEST_DB_DSN` | `postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable` |
| `WALLET_TEST_KEYCLOAK` | `http://localhost:8080` |
| `WALLET_TEST_SQS_ENDPOINT` | `http://localhost:4566` |

> [!NOTE]
> **Two groups, two databases — and why**
>
> `make test-integration` splits the suite into two groups, because they have
> different database needs:
>
> 1. `./test/integration/...` tests **across the wire**: it opens a wallet
>    *through the API* and checks the result *in the database*. It needs the
>    stack's own database (that is where the API writes). Do not force
>    `WALLET_TEST_DB_DSN` to another database here — every "open and read back"
>    assertion would fail with an empty result, which is a misconfiguration,
>    not a bug.
> 2. `./internal/postgres/...` and `./internal/transport/http/...` talk
>    **directly** to the database and exercise paths that persist state the
>    invariants would reject (e.g. `UpdateBalance` without the matching ledger
>    row). So they get `WALLET_TEST_DB_DSN` pointing at an **isolated,
>    throwaway** database provisioned by `scripts/test-db.sh`.
>
> Without that split, running `make test-integration` leaves residue in the
> stack database and `make load-verify` reports "stored balance equals ledger
> sum" etc. — a **false** red, test residue read as balance corruption.

> [!WARNING]
> **An `API_URL` on `8081`, not `8080`**
>
> The integration tests use `http://localhost:8081` by default. The containers
> expose the API on that port (`make up-multi` on 8081/8082/8083), so pointing
> at `8080` — the local binary port — makes the test fail or hit the wrong
> process.


### What integration covers

| Area | What it proves |
| :-- | :-- |
| Concurrency / race | Simultaneous wagers on the same wallet do not corrupt the balance. |
| Idempotency | The same key does not debit twice; a distinct key with a different body conflicts. |
| Double-entry | Every movement has a matching ledger entry. |
| Kill-after-commit | Killing the process after commit loses nothing and duplicates nothing. |
| Restart | Restarting resumes consumption and the outbox without repeating effects. |
| Resilience | An unavailable dependency degrades, it does not corrupt. |
| Flow | The complete path of a wager, end to end. |
| Journey | A sequence of operations as a player would perform them. |
| Messaging | Envelope, attributes and queue routing. |
| Outbox race | Two publishers do not publish the same event twice. |
| Reconcile worker | A planted divergence is detected by the sweep. |
| Trace continuity | The trace survives the queue crossing. |
| Keycloak and SQS policies | The declared policies are the effective ones. |
| Realm sync | The imported realm matches the realm file. |
| tofu/SQS sync | The queue declarations in tofu and boto do not diverge. |

> [!TIP]
> **Tests that read files, not services**
>
> `realm_sync_test.go` and `tofu_sqs_sync_test.go` compare **declarations**
> across files. They fail when someone edits one side and forgets the other —
> the kind of divergence no runtime test would catch.


## E2E

```sh
make e2e   # scripts/e2e.sh
```

The script brings the stack up, waits for the healthchecks to answer and only
then runs. **The teardown brings the stack down** at the end — including when the
test passes.

> [!WARNING]
> **`make e2e` leaves you without an environment**
>
> After a `make e2e`, run `make up` again before any interactive work. It is
> not a bug: e2e aims to be hermetic.


There are ~25 e2e tests according to the README, covering the real path HTTP →
SQS → worker → Postgres.

## Choosing the right layer

| You changed… | Run |
| :-- | :-- |
| Domain rules, balance computation | `make test` |
| A goroutine, a worker, `TxManager` | `make test`, then `make test-race` |
| SQL query, index, transaction | `make test-integration` |
| Event envelope, queue, policy | `make test-integration` |
| Realm, tofu, infra script | `make test` (sync tests) |
| A user flow end to end | `make e2e` |

See also: [Load testing](load-testing.md) ·
[Project structure](../contributing/project-structure.md) ·
[Troubleshooting](troubleshooting.md).
