> The service is organised as **hexagonal (ports & adapters)**:
> the domain knows nothing about the database, HTTP, queues or metrics — only
> the interfaces declared by the use cases. This page draws the layers and
> explains why dependencies always point inwards.

## The dependency rule

A single rule holds the design together: **inner code never imports outer
code**. The domain (`internal/domain`) has no infrastructure imports at all;
the service (`internal/service`) declares **ports** (interfaces) that
infrastructure implements.

```mermaid
flowchart TB
    subgraph Drivers["🌐 External actors"]
        P[Game provider  
OAuth2 client_credentials]
        C[SQS FIFO queue  
WagerTransactionRequested]
        O[OTLP collector]
    end

    subgraph Adapters_In["Inbound adapters"]
        HTTP["transport/http  
go-kit endpoints + codecs"]
        CONS["messaging/consumer  
SQS worker"]
        REL["messaging/relay  
outbox publisher"]
        REF["messaging/reference  
reference resolver"]
    end

    subgraph Core["🧠 Core (no infra dependencies)"]
        SVC["service/  
use cases + PORTS"]
        DOM["domain/  
Money · Wallet · WagerTransaction  
LedgerEntry · Events · Errors"]
    end

    subgraph Adapters_Out["Outbound adapters"]
        PG["postgres/  
pgx + Unit of Work"]
        SQS["sqs/  
publisher"]
        AUTH["auth/  
OIDC/JWKS validation"]
        MET["platform/metrics  
go-kit + Prometheus"]
    end

    P -->|HTTP + Bearer| HTTP
    C --> CONS
    HTTP --> SVC
    CONS --> SVC
    REL --> SVC
    REF --> SVC
    SVC --> DOM
    SVC --> PG
    REL --> SQS
    HTTP --> AUTH
    SVC -.->|Instrumentation port| MET
    MET -.->|OTLP| O
    SQS -->|integration-events.fifo| C

    classDef core fill:#2ea04322,stroke:#2ea043,stroke-width:2px
    classDef adapter fill:#425CC722,stroke:#425CC7
    classDef ext fill:#FF990022,stroke:#FF9900
    class SVC,DOM core
    class HTTP,CONS,REL,REF,PG,SQS,AUTH,MET adapter
    class P,C,O ext
```

## The ports

Ports live in `internal/service/ports.go` and `internal/messaging/ports.go`.
Each exists because there are **two legitimate implementations** — the real
one and a test double — or because the domain needs a service it must not
know about.

| Port | Role | Real implementation |
| :-- | :-- | :-- |
| `WalletRepository` | Persist and **lock** wallets | `postgres/wallet_repo.go` |
| `TransactionRepository` | Persist wagers and idempotency identity | `postgres/tx_repo.go` |
| `LedgerRepository` | Append-only accounting | `postgres/ledger_repo.go` |
| `InboxRepository` | Durable consumption deduplication | `postgres/inbox_repo.go` |
| `OutboxRepository` | Claim/publish events | `postgres/outbox_repo.go` |
| `TxManager` | Unit of Work (`WithinTx`) | `postgres/store.go` |
| `Instrumentation` | Observation (metrics and spans) | `platform/metrics` |
| `SpanCarrier` | Serialise the trace across the queue | `platform/otel` |

> [!NOTE]
> **Why `Instrumentation` is a port**
>
> Without it, `service/` would import the metrics library. With it, the use
> case reports **business outcomes** (`WagerOutcome`, `Duplicate`) and the
> composition root translates them into the go-kit registry. It is the
> *Observer* pattern applied to observability.


## Composition with Uber Fx

The composition root lives in `internal/bootstrap`. It holds **no business
rules**: it only wires modules, binds ports to adapters and registers
lifecycle hooks.

```mermaid
flowchart LR
    MAIN["cmd/walletd/main.go"] --> BOOT["bootstrap.Run"]
    BOOT --> M1["fx.Module: config"]
    BOOT --> M2["fx.Module: telemetry"]
    BOOT --> M3["fx.Module: postgres"]
    BOOT --> M4["fx.Module: service"]
    BOOT --> M5["fx.Module: transport"]
    BOOT --> M6["fx.Module: messaging"]
    M6 --> LC{"fx.Lifecycle"}
    LC -->|OnStart| W1["consumer.Run"]
    LC -->|OnStart| W2["relay.Run"]
    LC -->|OnStart| W3["reference.Run"]
    LC -->|OnStop| SD["ordered shutdown"]

    classDef boot fill:#1d5fa8,color:#fff
    classDef mod fill:#e8f0fa
    class BOOT,MAIN boot
    class M1,M2,M3,M4,M5,M6,LC,W1,W2,W3,SD mod
```

`fx.Lifecycle` is what makes **start and stop symmetric**: workers come up
after their dependencies and go down before them, in reverse order. It is also
what lets the process shut down without cutting an outbox publication in half.

## One binary, three modes

The same binary serves different roles depending on configuration, which keeps
three `main`s from diverging:

| Mode | Selected by | What starts |
| :-- | :-- | :-- |
| `all` | default | HTTP + consumer + relay + reference worker |
| `api` | `WALLET_MODE=api` | HTTP server only (probes and scraping stay up) |
| `worker` | `WALLET_MODE=worker` | background workers only |

This is what lets `make up-split` run the API and the workers in separate
processes without a second set of use cases — the split is one of **process**,
not of code.

See also: [Ports & adapters in depth](hexagonal.md) ·
[Wager flow](wager-flow.md) · [Invariants](invariants.md).
