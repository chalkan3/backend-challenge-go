> Documentation for the **wallet service for game providers**. The
> hard part of this project is not the arithmetic: it is guaranteeing that,
> under network retries, duplicate messages, multiple replicas and restarts,
> **money is never counted twice or lost**. Every page here
> describes one of those guarantees and how it is enforced — in the domain, in
> the database, or in both.

- **🚀 Getting started** — Requirements, bringing the stack up with one command, and your first wager.  
  [`getting-started/quickstart`](getting-started/quickstart.md)
- **🏗️ Architecture** — Ports & adapters, Uber Fx composition, and how a wager travels.  
  [`architecture/overview`](architecture/overview.md)
- **🔌 API** — Every endpoint, error codes, idempotency and curl examples.  
  [`api/reference`](api/reference.md)
- **📒 Data** — Schema, append-only ledger, transactional outbox and durable inbox.  
  [`data/ledger`](data/ledger.md)
- **✉️ Messaging** — SQS FIFO, deduplicating consumer, outbox relay and pending references.  
  [`messaging/overview`](messaging/overview.md)
- **🧪 Operations** — Configuration, observability, testing, load testing and troubleshooting.  
  [`operations/testing`](operations/testing.md)

## What this service guarantees

| Guarantee | How it is achieved |
| :-- | :-- |
| 🔁 **Idempotency** | Every operation carries an `Idempotency-Key`; the result is persisted and reproduced on replay |
| 📒 **Auditability** | **Append-only** ledger: each movement is an immutable row that sums up to the balance |
| ✉️ **Reliable delivery** | **Transactional outbox** — the event is written in the same transaction as the balance |
| 📥 **Duplicate-free consumption** | **Durable inbox** deduplicates by message identity |
| 🔒 **Concurrency** | Per-wallet row-level lock + optimistic `version`; distinct wallets progress in parallel |
| 🧮 **Monetary precision** | `Money` is a value object in minor units (`int64`), never a `float` |

## Documentation map

```mermaid
flowchart LR
    subgraph Entry["I want to..."]
        A[Understand the design] --> B[Architecture]
        C[Run the project] --> D[Getting started]
        E[Call the API] --> F[API]
        G[Operate / debug] --> H[Operations]
    end

    B --> B1[Overview]
    B --> B2[Hexagonal]
    B --> B3[Wager flow]
    B --> B4[Domain model]
    B --> B5[Invariants]

    D --> D1[Requirements]
    D --> D2[Bring up the stack]
    D --> D3[First wager]

    F --> F1[Endpoints]
    F --> F2[Errors]
    F --> F3[Idempotency]

    H --> H1[Configuration]
    H --> H2[Observability]
    H --> H3[Testing and load]

    classDef hub fill:#1d5fa8,color:#fff,stroke:#0b3d2c,stroke-width:2px
    classDef leaf fill:#e8f0fa,color:#0b3d2c,stroke:#1d5fa8
    class A,C,E,G,B,D,F,H hub
    class B1,B2,B3,B4,B5,D1,D2,D3,F1,F2,F3,H1,H2,H3 leaf
```

## Getting started in one command

```sh
make up          # brings up postgres, keycloak, localstack and the app
make test        # fast suite (no stack required)
make e2e         # end-to-end contract against the real stack
```

> [!TIP]
> **This site is bilingual**
>
> Use the language selector in the header to switch between **Português**
> and **English**. Content is mirrored page by page.


> [!NOTE]
> **The README remains the repository's entry point**
>
> This site is detailed documentation. The [`README.md`](https://github.com/chalkan3/backend-challenge-go)
> at the project root stays as the high-level overview and is the first thing
> someone cloning the repository sees.

