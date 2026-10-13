> The problems below are the ones that **actually happened** on this
> project. Each one carries the exact symptom, the measured cause and the command
> that fixes it. Where a behaviour looks like an error but is a contract, that is
> stated explicitly.

> [!TIP]
> **Before blaming the code**
>
> A `make preflight` costs two seconds and eliminates half of this page. The
> other half is usually LocalStack, with measured and documented limits.


## Environment lifecycle

### Port already in use

**Symptom:** startup fails immediately with the port already taken.

**Cause:** another stack (a `kind` cluster, a previous run) is holding the port.

**Fix:** run the preflight, which checks the ports before bringing anything up.

```sh
make preflight        # scripts/port-check.sh
```

| Port | Who uses it |
| :-- | :-- |
| `5433` | Postgres. |
| `8080` | Local API / Keycloak. |
| `4566` | LocalStack (SQS). |
| `8081` | API in the containers / multi-replica. |

### `make e2e` tears the stack down

**Symptom:** after a successful `make e2e`, nothing answers any more.

**Cause:** e2e is **hermetic on purpose** — it brings the stack up, runs and
tears it down, including when it passes.

**Fix:** bring it back with `make up`, or ask it to keep the stack.

```sh
make up                  # bring the environment back
make e2e KEEP=1          # run e2e and leave the stack up
UP_ONLY=1 ./scripts/e2e.sh   # bring the stack up and stop (handy for k6 to drive)
```

### `podman-compose down` fails with active profiles

**Symptom:** `down` complains about services that belong to a profile.

**Cause:** a known `podman-compose` issue with profile services.

**Fix:** remove the containers by force.

```sh
podman rm -f 
```

### `up` does not recreate because the image tag is unchanged

**Symptom:** you changed the code, `make up` runs, but the behaviour is the old
one.

**Cause:** the image tag is the same, so the engine does not recreate the
container.

**Fix:** force the recreation.

```sh
docker compose up -d --force-recreate
```

## Services with traps

### Keycloak `--import-realm` uses `IGNORE_EXISTING`

**Symptom:** the realm in Keycloak does not reflect `realm-wallet.json`.

**Cause:** `--import-realm` uses the **`IGNORE_EXISTING`** strategy: an
*existing* realm is **not** reimported. Editing the JSON and restarting does
**not** apply the change.

**Detection:** `realm_sync_test.go` fails **loudly** against a stale realm — this
is the expected symptom, not a false positive.

**Fix:** remove the realm/volume before reimporting.

> [!WARNING]
> **It is not a Keycloak bug — it is `IGNORE_EXISTING` semantics**
>
> The test exists precisely because this trap is otherwise silent: without it,
> the stale realm would slip through.


### LocalStack: measured limits

| Symptom | Measured cause | Way out |
| :-- | :-- | :-- |
| `Deny * on sqs:*` blocks nothing | The resource policy is **stored but not enforced** in the community edition | Write the policy; know that it only holds on real AWS |
| `create_queue` "does not apply" the attribute change | `create_queue` is **not idempotent** for an attribute change | `get_queue_url` + `set_queue_attributes` (what the script does) |
| IAM/STS/KMS unavailable | They are `disabled` in `_localstack/health` | Do not rely on CMKs or per-service IAM locally |

> [!NOTE]
> **Tofu is not the way out of LocalStack**
>
> The policies in `deploy/tofu` describe the shape they **should** have on AWS.
> See [Infrastructure as code](infrastructure.md): it is a declaration, not a
> local proof.


## Responses that look like errors but are the contract

These are made on purpose by **correct** behaviour. Before filing a bug, check
the reading column.

| Response | Reading | Action |
| :-- | :-- | :-- |
| `503` on `/health/ready` | Postgres or SQS unreachable — that is **the endpoint's contract** | Fix the dependency, not the endpoint |
| `409 IDEMPOTENCY_KEY_CONFLICT` | The **same key** arrived with a **different body** | Fix the client that reused the key |
| `202 PENDING_REFERENCE` | A `REFUND`/`ROLLBACK` referencing a transaction that **does not exist yet** | **Not an error**: the reference worker resolves it |
| `422 INSUFFICIENT_FUNDS` | A **persisted** business rejection | Nothing to fix; the rejection is recorded |

> [!NOTE]
> **`202 PENDING_REFERENCE` is the case most often mistaken for a failure**
>
> A refund can arrive **before** the transaction it reverses — the FIFO queue
> guarantees order within a group, but the two events may be generated at
> different moments. The service accepts it, marks it pending and resolves it
> later. Answering `2xx` there is the correct behaviour.


> [!WARNING]
> **`422` is persisted too**
>
> The insufficient-funds rejection is **not** a volatile validation: it is
> recorded. If a test expects "nothing was written", the test is wrong.


### Quick response diagnosis

```sh
# 503: readiness is talking about a dependency, not the process.
curl -s localhost:8080/health/ready

# 409: check whether the reused key carried the same body.
curl -si -X POST localhost:8080/v1/wallets/$ID/transactions \
  -H "Idempotency-Key: $KEY" -d @body.json

# 202: the referenced transaction does not exist yet; wait for the worker.
curl -s localhost:8080/v1/transactions/$REF
```

## Telemetry signals as diagnosis

When the problem is not the environment, the answer is usually in the metrics.

| Metric | Reading |
| :-- | :-- |
| `wallet_outbox_pending` climbing | The relay is not draining: look at `WALLET_OUTBOX_INTERVAL` and `WALLET_OUTBOX_BATCH`. |
| `wallet_outbox_lag_seconds` high | An old event is stuck — a downstream consumer may be failing. |
| `wallet_consumer_retries_total` climbing | Messages being reprocessed; check visibility (`WALLET_SQS_VISIBILITY`). |
| `wallet_consumer_dlq_total` > 0 | A poisoned message; the payload is in the DLQ for inspection. |
| `wallet_reconciliation_divergences_total` > 0 | **A correctness bug** — stop and investigate. |
| `wallet_auth_failures_total` by reason | The issuer/JWKS or the `aud` are out of alignment. |

```sh
# The numbers, without a dashboard.
curl -s localhost:8080/metrics | grep ^wallet_
```

See also: [Configuration](configuration.md) ·
[Observability](observability.md) ·
[Infrastructure](infrastructure.md) ·
[Testing](testing.md).
