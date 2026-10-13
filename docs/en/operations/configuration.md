> All service configuration comes in through **environment variables**,
> read exactly once at startup and validated before the process comes up. The file
> `internal/platform/config/config.go` is the source of truth: this page
> documents every variable, its real *default* and the effect of changing
> it.

## The rule: working defaults, fail early

`config.Load()` reads the environment, applies the *defaults* and runs
`validate()`. If anything is invalid, Fx **refuses to start** — there is no
partial boot with half-applied configuration.

> [!NOTE]
> **Why validate at load, not at use**
>
> An empty DSN discovered only on the first query becomes an intermittent
> error under load. Validating at startup turns the error into a **boot
> failure**, which is immediate, reproducible and visible in
> `docker compose logs`.


```sh
# The binary reads the environment; nothing is read from a file by default.
WALLET_MODE=api WALLET_LOG_LEVEL=debug ./walletd
```

> [!TIP]
> **Defaults that work locally**
>
> The *defaults* point at the `docker-compose.yml` stack (Postgres on `5433`,
> LocalStack on `4566`, Keycloak on `8080`, API on `8080`). Running `make up`
> and then the binary **with no variables at all** is already a complete
> environment.


## Application and lifecycle

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_MODE` | `all` | Selects `all`, `api` or `worker`. See the mode table below. |
| `WALLET_HTTP_ADDR` | `:8080` | HTTP listen address. Required: empty is a boot error. |
| `WALLET_SHUTDOWN_GRACE` | `20s` | Window to stop workers and in-flight work before `SIGKILL`. |
| `WALLET_LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. Any other value is a boot error. |

| `WALLET_MODE` | What starts |
| :-- | :-- |
| `all` | HTTP + consumer + outbox relay + reference worker + reconcile |
| `api` | HTTP server only (probes and `/metrics` stay up) |
| `worker` | Background workers only |

> [!WARNING]
> **`WALLET_WORKERS_ENABLED` is independent of `WALLET_MODE`**
>
> The mode decides **what the process may** run; `WALLET_WORKERS_ENABLED=false`
> turns the workers off even in `all`. Use the former to split processes, the
> latter to silence the workers for a one-off test.


## Database

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_DB_DSN` | `postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable` | pgx connection. Required: empty is a boot error. |
| `WALLET_DB_MAX_CONNS` | `20` | Connection pool ceiling. Must be `>= 1`. |
| `WALLET_MIGRATE_ON_START` | `true` | Applies the `internal/migrations` migrations on startup. |

> [!NOTE]
> **`WALLET_DB_MAX_CONNS` versus `WALLET_CONSUMER_CONCURRENCY`**
>
> Every consumed message opens a transaction and holds a connection while it
> runs. With 4 consumers, a pool of 20 is plenty; with 32 consumers, a pool of
> 20 makes the pool queue the bottleneck — and the symptom is latency, not an
> error.


## SQS

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_SQS_ENDPOINT` | `http://localhost:4566` | SQS endpoint (local LocalStack, AWS in the cloud). |
| `WALLET_SQS_REGION` | `us-east-1` | Client region. Required. |
| `WALLET_SQS_ACCESS_KEY_ID` | `test` | Static credential (LocalStack accepts any value). |
| `WALLET_SQS_SECRET_KEY` | `test` | Pair of the credential above. |
| `WALLET_SQS_REQUEST_QUEUE` | `wager-transactions.fifo` | Wager inbound queue. Must end in `.fifo`. |
| `WALLET_SQS_EVENTS_QUEUE` | `integration-events.fifo` | Outbox outbound queue. Must end in `.fifo`. |
| `WALLET_SQS_DLQ_QUEUE` | `wager-transactions-dlq.fifo` | Dead-letter queue for `wager-transactions.fifo`. |
| `WALLET_SQS_WAIT_SECONDS` | `5` | Long polling, within `[0,20]`. |
| `WALLET_SQS_MAX_MESSAGES` | `10` | Batch size per `ReceiveMessage`, within `[1,10]`. |
| `WALLET_SQS_VISIBILITY` | `60` | Visibility timeout in seconds while processing. |
| `WALLET_SQS_MAX_RECEIVE` | `5` | Receives before the message moves to the DLQ (redrive). |

> [!WARNING]
> **All three queues must exist**
>
> Validation requires `REQUEST_QUEUE` and `EVENTS_QUEUE` to have the `.fifo`
> suffix; a name without `.fifo` is a **boot error**, not a warning. The queues
> themselves are created by `deploy/localstack/init/10-queues.py` (via
> `make up`) or by `deploy/tofu` — not by the binary.


## Workers

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_WORKERS_ENABLED` | `true` | Turns all background workers on/off. |
| `WALLET_CONSUMER_CONCURRENCY` | `4` | Goroutines consuming the request queue in parallel. |
| `WALLET_OUTBOX_INTERVAL` | `500ms` | Period of the outbox relay tick. |
| `WALLET_OUTBOX_BATCH` | `100` | Maximum events claimed per tick. |
| `WALLET_OUTBOX_LEASE` | `30s` | Claim ownership before another publisher may take it over. |
| `WALLET_REFERENCE_INTERVAL` | `1s` | Period of the reference worker tick. |
| `WALLET_REFERENCE_BATCH` | `50` | Maximum references resolved per tick. |
| `WALLET_REFERENCE_MAX_ATTEMPTS` | `10` | Attempts before a reference becomes a permanent failure. |
| `WALLET_REFERENCE_TTL` | `15m` | Age ceiling for a pending reference before it expires. |
| `WALLET_RECONCILE_INTERVAL` | `5m` | Period of the reconciliation sweep. |
| `WALLET_RECONCILE_BATCH` | `100` | Wallets per batch in the sweep (memory and read duration). |

> [!NOTE]
> **Small interval, not zero**
>
> The outbox relay is *polling* with a `500ms` period. The event is written in
> the **same transaction** as the wager and only then published — the interval
> does not affect correctness, only the latency until the event leaves. Lowering
> it reduces latency and increases the number of empty queries.


## Authentication

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_AUTH_ISSUER` | `http://localhost:8080/realms/wallet` | OIDC issuer expected in `iss`. Required. |
| `WALLET_AUTH_JWKS_URL` | *(empty)* | When empty, derived from the issuer. Fill it only when the JWKS is non-standard. |
| `WALLET_AUTH_AUDIENCE` | *(empty)* | `aud` check. Empty **disables** the check; the compose sets `wallet-api`. |

> [!WARNING]
> **`WALLET_AUTH_ISSUER` is mandatory**
>
> OAuth2/OIDC is not optional: without an issuer the service does not come up.
> This is a design decision — authentication is a requirement, not a *feature
> flag*.


## Pagination

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_LEDGER_PAGE_MAX` | `200` | Items per page ceiling in the statement. Bounds the cost of a listing. |

## OpenTelemetry

Telemetry is **opt-in**: with `WALLET_OTEL_ENABLED=false` the service runs exactly
as before and no environment depends on a running collector.

| Variable | Default | Effect |
| :-- | :-- | :-- |
| `WALLET_OTEL_ENABLED` | `false` | Enables OTLP traces/logs. Disabled, the exporter is a no-op. |
| `WALLET_OTEL_ENDPOINT` | `localhost:4317` | Collector host:port (OTLP/gRPC). |
| `WALLET_OTEL_INSECURE` | `true` | No TLS on export (correct for the local collector). |
| `WALLET_OTEL_SERVICE_NAME` | `wallet` | `service.name` value in the resources. |
| `WALLET_ENV` | `local` | `deployment.environment` value. |
| `WALLET_INSTANCE_ID` | *hostname* | Distinguishes replicas; falling back to the hostname keeps the value stable. |
| `WALLET_OTEL_SAMPLE_RATIO` | `1.0` | Sampled fraction. Reduce under high-volume production. |
| `WALLET_OTEL_LOGS_ENABLED` | `true` | Exports logs to Loki through the collector. |
| `WALLET_OTEL_EXPORT_TIMEOUT` | `5s` | Timeout per export. |
| `WALLET_OTEL_BATCH_TIMEOUT` | `2s` | BatchSpanProcessor interval. |
| `WALLET_OTEL_MAX_QUEUE_SIZE` | `4096` | Processor queue ceiling. |

> [!TIP]
> **Turning telemetry off does not change behaviour**
>
> Spans are recorded through the `Instrumentation` port, whose no-op costs no
> network. That is what lets the fast suite run **without** the observability
> stack up.


## A complete environment, from scratch

```sh
# Brings up Postgres, LocalStack (queues) and Keycloak.
make up

# The binary runs on the defaults; nothing to export.
go run ./cmd/walletd

# Isolated worker mode, pointing at a remote Postgres:
WALLET_MODE=worker \
WALLET_DB_DSN="postgres://wallet:wallet@db.internal:5432/wallet?sslmode=require" \
WALLET_SQS_ENDPOINT="https://sqs.us-east-1.amazonaws.com" \
WALLET_OTEL_ENABLED=true \
WALLET_OTEL_ENDPOINT="otel.internal:4317" \
./walletd
```

See also: [Observability](observability.md) ·
[Infrastructure as code](infrastructure.md) ·
[Troubleshooting](troubleshooting.md).
