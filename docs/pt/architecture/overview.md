> O serviço é organizado como **hexagonal (ports & adapters)**: o
> domínio não conhece banco, HTTP, fila nem métricas — apenas interfaces
> declaradas nos casos de uso. Esta página desenha as camadas e explica por que a
> dependência aponta sempre para dentro.

## A regra de dependência

Uma única regra sustenta o desenho: **o código de dentro nunca importa o código
de fora**. O domínio (`internal/domain`) não tem import de infraestrutura
alguma; o serviço (`internal/service`) declara **portas** (interfaces) que a
infraestrutura implementa.

```mermaid
flowchart TB
    subgraph Drivers["🌐 Atores externos"]
        P[Provedor de jogos  
OAuth2 client_credentials]
        C[Fila SQS FIFO  
WagerTransactionRequested]
        O[Collector OTLP]
    end

    subgraph Adapters_In["Adapters de entrada"]
        HTTP["transport/http  
go-kit endpoints + codecs"]
        CONS["messaging/consumer  
worker SQS"]
        REL["messaging/relay  
outbox publisher"]
        REF["messaging/reference  
resolvedor de referências"]
    end

    subgraph Core["🧠 Núcleo (sem dependências de infra)"]
        SVC["service/  
casos de uso + PORTS"]
        DOM["domain/  
Money · Wallet · WagerTransaction  
LedgerEntry · Eventos · Erros"]
    end

    subgraph Adapters_Out["Adapters de saída"]
        PG["postgres/  
pgx + Unit of Work"]
        SQS["sqs/  
publisher"]
        AUTH["auth/  
validação OIDC/JWKS"]
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
    SVC -.->|port Instrumentation| MET
    MET -.->|OTLP| O
    SQS -->|integration-events.fifo| C

    classDef core fill:#2ea04322,stroke:#2ea043,stroke-width:2px
    classDef adapter fill:#425CC722,stroke:#425CC7
    classDef ext fill:#FF990022,stroke:#FF9900
    class SVC,DOM core
    class HTTP,CONS,REL,REF,PG,SQS,AUTH,MET adapter
    class P,C,O ext
```

## As portas

As portas ficam em `internal/service/ports.go` e `internal/messaging/ports.go`.
Cada uma existe porque há **duas implementações legítimas** — a real e a de
teste — ou porque o domínio precisa de um serviço que ele não pode conhecer.

| Porta | Papel | Implementação real |
| :-- | :-- | :-- |
| `WalletRepository` | Persistir e **travar** carteiras | `postgres/wallet_repo.go` |
| `TransactionRepository` | Persistir apostas e identidade de idempotência | `postgres/tx_repo.go` |
| `LedgerRepository` | Contabilidade append-only | `postgres/ledger_repo.go` |
| `InboxRepository` | Deduplicação de consumo durável | `postgres/inbox_repo.go` |
| `OutboxRepository` | Claim/publicação de eventos | `postgres/outbox_repo.go` |
| `TxManager` | Unit of Work (`WithinTx`) | `postgres/store.go` |
| `Instrumentation` | Observação (métricas e spans) | `platform/metrics` |
| `SpanCarrier` | Serializar o trace através da fila | `platform/otel` |

> [!NOTE]
> **Por que `Instrumentation` é uma porta**
>
> Sem ela, `service/` importaria a biblioteca de métricas. Com ela, o
> caso de uso reporta **resultados de negócio** (`WagerOutcome`,
> `Duplicate`) e o *composition root* traduz para o registro do go-kit.
> É o padrão *Observer* aplicado à observabilidade.


## Composição com Uber Fx

O *composition root* fica em `internal/bootstrap`. Ele **não contém regra de
negócio**: apenas monta módulos, liga portas a adapters e registra ciclo de
vida.

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
    LC -->|OnStop| SD["shutdown ordenado"]

    classDef boot fill:#1d5fa8,color:#fff
    classDef mod fill:#e8f0fa
    class BOOT,MAIN boot
    class M1,M2,M3,M4,M5,M6,LC,W1,W2,W3,SD mod
```

O `fx.Lifecycle` é o que torna **start e stop simétricos**: os workers sobem
depois das dependências e descem antes delas, na ordem inversa. É também o que
permite o processo encerrar sem cortar uma publicação do outbox no meio.

## Um binário, três modos

O mesmo binário atende papéis diferentes conforme a configuração, o que evita
três `main`s divergindo:

| Modo | Como é selecionado | O que sobe |
| :-- | :-- | :-- |
| `all` | padrão | HTTP + consumer + relay + reference worker |
| `api` | `WALLET_MODE=api` | só o servidor HTTP (probes e scraping continuam) |
| `worker` | `WALLET_MODE=worker` | só os workers em background |

Isso é o que permite `make up-split` rodar API e workers em processos separados
sem um segundo conjunto de casos de uso — a divisão é de **processo**, não de
código.

Veja também: [Ports & Adapters em detalhe](hexagonal.md) ·
[Fluxo de uma aposta](wager-flow.md) · [Invariantes](invariants.md).
