<div align="center">

# 💳 Wallet — Desafio Backend

**Serviço de carteiras para operações financeiras de provedores de jogos**, com
garantias distribuídas: idempotência persistente, ledger append-only,
transactional outbox, inbox durável e controle de concorrência por carteira.

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![go-kit](https://img.shields.io/badge/transport-go--kit-00ACD7)](https://github.com/go-kit/kit)
[![Uber Fx](https://img.shields.io/badge/composition-Uber%20Fx-000000?logo=uber&logoColor=white)](https://github.com/uber-go/fx)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)
[![AWS SQS](https://img.shields.io/badge/AWS%20SQS-FIFO-FF9900?logo=amazonsqs&logoColor=white)](https://aws.amazon.com/sqs/)
[![Keycloak](https://img.shields.io/badge/Keycloak-OIDC-4D4D4D?logo=keycloak&logoColor=white)](https://www.keycloak.org)
[![OpenTelemetry](https://img.shields.io/badge/OpenTelemetry-traces-425CC7?logo=opentelemetry&logoColor=white)](https://opentelemetry.io)
[![Docker](https://img.shields.io/badge/containers-podman%20%7C%20docker-2496ED?logo=docker&logoColor=white)](https://podman.io)

[![CI](https://github.com/chalkan3/backend-challenge-go/actions/workflows/ci.yml/badge.svg)](https://github.com/chalkan3/backend-challenge-go/actions/workflows/ci.yml)
[![Release](https://github.com/chalkan3/backend-challenge-go/actions/workflows/release.yml/badge.svg)](https://github.com/chalkan3/backend-challenge-go/actions/workflows/release.yml)
[![Testes unitários](https://img.shields.io/badge/unit%20tests-71%20passing-2ea043)](#-testes)
[![Testes e2e](https://img.shields.io/badge/e2e%20tests-25%20passing-2ea043)](#-testes)
[![Go Report Card](https://goreportcard.com/badge/github.com/chalkan3/backend-challenge-go)](https://goreportcard.com/report/github.com/chalkan3/backend-challenge-go)

</div>

---

## 📑 Índice

- [Visão geral](#-visão-geral)
- [Arquitetura](#-arquitetura)
- [Como uma aposta percorre o sistema](#-como-uma-aposta-percorre-o-sistema)
- [Stack](#-stack)
- [Estrutura do projeto](#-estrutura-do-projeto)
- [Subindo o ambiente](#-subindo-o-ambiente)
  - [Um binário, três modos](#um-binário-três-modos-e-por-que-não-três-binários)
  - [Kubernetes local (kind)](#kubernetes-local-kind--a-segunda-forma-de-subir-o-ambiente)
  - [Caminho GitOps (opcional): ArgoCD](#caminho-gitops-opcional-argocd)
- [Observabilidade](#-observabilidade)
- [API](#-api)
- [Testes](#-testes)
  - [Testes de carga (k6)](#testes-de-carga-k6)
- [Integração contínua](#-integração-contínua)
- [Simulações de falha](#-simulações-de-falha)
- [Decisões técnicas](#-decisões-técnicas)

---

## 🎯 Visão geral

Um provedor de jogos chama este serviço para **debitar e creditar a carteira de
um jogador**. A dificuldade não está na aritmética — está em garantir que, sob
retry de rede, mensagens duplicadas, múltiplas réplicas e reinícios, **dinheiro
nunca seja contado duas vezes nem perdido**.

| Garantia | Como é obtida |
| :-- | :-- |
| 🔁 **Idempotência** | Toda operação carrega `Idempotency-Key`; o resultado é persistido e replicado em replay |
| 📒 **Auditabilidade** | Ledger **append-only**: cada movimento é uma linha imutável que soma até o saldo |
| ✉️ **Entrega confiável** | **Transactional outbox** — o evento é gravado na mesma transação do saldo |
| 📥 **Consumo sem duplicação** | **Inbox durável** deduplica por identidade da mensagem |
| 🔒 **Concorrência** | Lock row-level por carteira + `version` otimista; carteiras distintas progridem em paralelo |
| 🧮 **Precisão monetária** | `Money` é um value object em unidades menores (`int64`), nunca `float` |

---

## 🏗️ Arquitetura

Layout **hexagonal (ports & adapters)**: o domínio não conhece banco, HTTP,
fila nem métricas — apenas interfaces declaradas nos casos de uso.

```mermaid
flowchart TB
    subgraph Drivers["🌐 Atores externos"]
        P[Provedor de jogos<br/>OAuth2 client_credentials]
        C[Fila SQS FIFO<br/>WagerTransactionRequested]
        O[Collector OTLP]
    end

    subgraph Adapters_In["Adapters de entrada"]
        HTTP["transport/http<br/>go-kit endpoints + codecs"]
        CONS["messaging/consumer<br/>worker SQS"]
        REL["messaging/relay<br/>outbox publisher"]
        REF["messaging/reference<br/>resolvedor de referências"]
    end

    subgraph Core["🧠 Núcleo (sem dependências de infra)"]
        SVC["service/<br/>casos de uso + PORTS"]
        DOM["domain/<br/>Money · Wallet · WagerTransaction<br/>LedgerEntry · Eventos · Erros"]
    end

    subgraph Adapters_Out["Adapters de saída"]
        PG["postgres/<br/>pgx + Unit of Work"]
        SQS["sqs/<br/>publisher"]
        AUTH["auth/<br/>validação OIDC/JWKS"]
        MET["platform/metrics<br/>go-kit + Prometheus"]
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

**Composição com Uber Fx.** O *composition root* (`internal/bootstrap`) monta
`fx.Module`s por área; `fx.Lifecycle` controla start/stop dos workers e do
servidor. O go-kit complementa sem conflito: `kit/endpoint` isola os casos de
uso do transporte e os middlewares (`authorize`, `instrument`) são Decorators.

### Padrões de projeto aplicados

| Padrão | Onde |
| :-- | :-- |
| **Ports & Adapters** | `internal/domain` + `internal/service/ports.go` |
| **Repository** | `internal/postgres/*_repo.go` |
| **Unit of Work** | `TxManager` — tudo de uma operação comita junto ou nada |
| **Decorator** | Endpoint middlewares, HTTP middlewares, access log |
| **Strategy** | Dispatch por `TxKind` (BET/WIN/LOSS/REFUND/ROLLBACK) |
| **State** | Máquina de estados da `WagerTransaction` |
| **Observer** | Port de instrumentação → métricas/traces |
| **Adapter** | Instrumentação → kit/metrics; SQS/pgx/OIDC |
| **Factory** | Envelopes de eventos, rotas, métricas |
| **Functional Options** | `WithReferencePolicy`, `WithLedgerPageSize` |

---

## 🔄 Como uma aposta percorre o sistema

```mermaid
sequenceDiagram
    autonumber
    participant P as Provedor
    participant API as HTTP (go-kit)
    participant SVC as WagerService
    participant DB as PostgreSQL
    participant REL as Outbox relay
    participant MQ as integration-events.fifo

    P->>API: POST /wagering/transactions<br/>Idempotency-Key + Bearer
    API->>API: valida JWT (JWKS) e autoriza provider
    API->>SVC: Execute(BET 30.00)

    rect rgb(46,160,67,0.12)
    note over SVC,DB: uma única transação de banco
    SVC->>DB: BEGIN
    SVC->>DB: SELECT wallet ... FOR UPDATE
    SVC->>DB: INSERT wager_transactions
    SVC->>DB: INSERT wallet_ledger_entries (append-only)
    SVC->>DB: UPDATE wallets SET balance, version
    SVC->>DB: INSERT integration_outbox
    SVC->>DB: COMMIT
    end

    SVC-->>API: status=PROCESSED, saldo atualizado
    API-->>P: 200 OK + transactionId

    REL->>DB: SELECT * FROM integration_outbox<br/>WHERE published_at IS NULL<br/>FOR UPDATE SKIP LOCKED
    REL->>MQ: SendMessage(eventId, payload)
    REL->>DB: UPDATE ... SET published_at = now()
    note over REL,MQ: publish no commit — nunca antes
```

> [!IMPORTANT]
> O saldo **nunca** é a fonte da verdade: ele é um *cache derivado* do ledger.
> `POST /wallets/{id}/reconciliation` recomputa a soma dos lançamentos e
> compara — o teste e2e verifica isso a cada jornada.

---

## 🧰 Stack

| Responsabilidade | Escolha |
| :-- | :-- |
| **Linguagem** | Go 1.26 (`go.mod` e `Dockerfile` declaram a versão) |
| **Composição** | Uber Fx (`fx.Module`, `fx.Provide`, `fx.Invoke`, `fx.Lifecycle`) |
| **Transport** | go-kit (`kit/endpoint` + `kit/transport/http` + `kit/metrics`) sobre `net/http` |
| **Persistência** | PostgreSQL 17 com `pgx/v5` e SQL explícito |
| **Mensageria** | AWS SQS (FIFO) via LocalStack |
| **IdP** | Keycloak (OAuth2/OIDC, `client_credentials`) |
| **Migrations** | goose embarcadas no binário (`go:embed`) |
| **Métricas** | go-kit metrics + provider Prometheus em `/metrics`; logs JSON (`slog`) |
| **Traces** | OpenTelemetry (OTLP/gRPC) → Collector → Tempo; dashboards Grafana como código |
| **Testes** | testify (`require`/`assert`), table-driven |

---

## 📂 Estrutura do projeto

```text
cmd/walletd/            # binário: fx.New(bootstrap.App) + subcomando migrate
internal/
  domain/               # modelo puro: Money (value object), Wallet (aggregate),
                        # WagerTransaction (state machine), LedgerEntry, eventos, erros
  service/              # casos de uso (aplicação) + PORTS (repositórios, TxManager,
                        # Instrumentation); sem pgx/HTTP/SQS/métricas
  transport/http/       # go-kit HTTP: Endpoints, codecs, error encoder, middlewares
  messaging/            # workers de fundo: consumer SQS, outbox relay, reference resolver
  postgres/             # adapter pgx: pool, migrations, repositórios (Repository + UoW)
  sqs/                  # adapter AWS SQS: client + publisher da outbox
  auth/                 # adapter OIDC (jwx/JWKS): validação de bearer tokens
  platform/             # config, logging (slog), metrics (kit/metrics + Prometheus), otel
  bootstrap/            # composition root: fx.Modules + adapter de instrumentação
  migrations/           # SQL goose embarcado (go:embed)
test/integration/       # suíte end-to-end contra Postgres/Keycloak/LocalStack reais
                        # (inclui journey_test.go: histórias completas do produto)
loadtest/               # testes de carga em k6 + auditoria de integridade em SQL
  k6/scenarios/         # smoke, hot-wallet, idempotency, mixed, distributed, soak
  sql/invariants.sql    # invariantes contábeis, rodadas direto no Postgres
scripts/e2e.sh          # runner: sobe a stack, espera health, roda a suíte, derruba
deploy/
  observability/        # otel-collector, prometheus, tempo, loki + provisioning Grafana
  keycloak/             # realm-wallet.json (import automático)
  localstack/           # init: criação das filas FIFO
```

---

## 🚀 Subindo o ambiente

**Pré-requisitos:** Go 1.26+, e Docker (ou Podman) + Compose.

```sh
make up            # equivale a `docker compose up --build -d`
```

> [!TIP]
> O projeto foi validado com **Podman 5 + podman-compose**. Os comandos deste
> README usam `docker compose` por ser o padrão do desafio, mas **você não
> precisa trocar nada à mão**: `make up`, `make down`, `make logs` e `make e2e`
> detectam a implementação disponível (`podman-compose` ou `docker compose`) via
> `scripts/engine.sh` — a mesma fonte de verdade para todos os entry points. Se
> preferir invocar o compose direto, use a implementação do seu host.
>
> Os alvos `make up*` rodam antes um **pre-flight de portas**
> (`scripts/port-check.sh`): se algo de fora do projeto já ocupa 8080/8081/4566
> — tipicamente um cluster `kind`, que publica exatamente esses números — o
> comando falha na hora com a causa, em vez de deixar os containers presos em
> `Created`. Nesse caso rode `make kind-down` antes.


### Portas publicadas

| Serviço | Endereço | Observação |
| :-- | :-- | :-- |
| 🟢 **API** | http://localhost:8081 | serviço wallet |
| 🔐 Keycloak | http://localhost:8080 | realm `wallet` (admin/admin) |
| 📬 LocalStack | http://localhost:4566 | SQS FIFO |
| 🐘 PostgreSQL | localhost:5433 | `wallet`/`wallet` |
| 📊 Grafana | http://localhost:3000 | só com `--profile observability` |

Com `--profile split` (a mesma imagem em dois papéis):

| Serviço | Endereço | Papel |
| :-- | :-- | :-- |
| 🟢 **API** | http://localhost:8084 | `WALLET_MODE=api` — rotas de negócio, pool dedicado |
| ⚙️ **Workers** | http://localhost:8085 | `WALLET_MODE=workers` — só `/health/*` e `/metrics` |

O container da aplicação **aplica as migrations no start**, importa o realm
`wallet` (automático) e cria as filas SQS via script de init do LocalStack.

### Réplicas múltiplas (concorrência distribuída)

```sh
docker compose --profile multi up --build -d   # app-1 (:8081), app-2 (:8082), app-3 (:8083)
```

### Um binário, três modos (e por que não três binários)

`WALLET_MODE` seleciona os papéis que **este processo** executa. É a mesma
imagem, o mesmo grafo Fx — só muda o que é iniciado:

| `WALLET_MODE` | HTTP de negócio | Workers | Migrations no start |
| :-- | :--: | :--: | :--: |
| `all` (padrão) | ✅ | ✅ | ✅ |
| `api` | ✅ | ❌ | ✅ |
| `workers` | ❌ (só `/health/*` e `/metrics`) | ✅ | ❌ |

```sh
docker compose --profile split up --build -d   # api :8084  +  workers :8085
```

**Por que um binário com modos, e não três binários.** A pergunta que importa
não é "dá para separar?", e sim "separar compra alguma coisa?". Compra — mas
não a que parece à primeira vista:

- **Throughput não.** A concorrência do consumer é **por processo**
  (`sem := make(chan struct{}, cfg.ConsumerConcurrency)` em `consumer.go`).
  Duas réplicas de um binário `workers` e duas réplicas de um binário
  combinado entregam exatamente a mesma capacidade. Separar não deu
  paralelismo novo, só mudou o endereço dele.
- **Isolamento de recursos, sim — e é o motivo real.** O pool pgx é
  compartilhado entre API e workers (`WALLET_DB_MAX_CONNS`). Um backlog de
  fila faz o consumer segurar conexões e pode **estrangurar o tráfego HTTP** no
  mesmo processo. No modo `api` o pool pertence inteiro ao request handling
  (`DB_MAX_CONNS=15`), no modo `workers` ele é dimensionado para a fila
  (`DB_MAX_CONNS=5`) — e cada lado escala na sua dimensão: API por latência,
  workers por profundidade de fila (`ApproximateNumberOfMessages`).
- **Ciclo de vida, sim.** Workers drenam no `SIGTERM` dentro de um grace
  period; separar permite dar ao `api` um grace curto (rotação rápida) e aos
  workers um grace longo (terminar o lote em voo) sem negociar um valor único.
- **Migrations, sim.** No modo `workers` elas **não** rodam: N consumers não
  devem disputar a tabela do goose no boot. O schema é do passo de deploy
  (ou do `migrate` subcommand).

**O que três binários custariam sem devolver nada:** três grafos Fx para
manter em sincronia (com testes de paridade por grafo), três healthchecks,
três conjuntos de config, e — o ponto decisivo — **a coordenação distribuída
não desaparece**. Consumer e outbox relay já disputam work com
`FOR UPDATE SKIP LOCKED` e o inbox é idempotente por `(consumer_name,
message_id)`. Pôr cada um em seu processo mantém essa mesma coordenação, só que
agora sobre a rede.

**Quando separar de verdade (os gatilhos):** quando escalar junto passa a
**causar dano mensurável** — um backlog de fila que exaure o pool e eleva o
P99 da API (o que o modo `api` já resolve sem segundo binário), quando os dois
lados precisam de **cadência de release diferente**, ou quando rodam em **nós
distintos** por política de recursos. Nenhum desses justifica hoje 3 binários;
todos são atendidos por `WALLET_MODE` + `--scale`.

```sh
# escala independente, uma dimensão por lado
docker compose --profile split up -d --scale workers=4
```

**Prova de que funciona** (`make e2e SPLIT=1`, 25 testes, race detector):

- `POST /wallets` no `api` (:8084) → **201**; no `workers` (:8085) → **404**;
- `/health/live`, `/health/ready` e `/metrics` respondem nos **dois** modos;
- uma BET enviada ao `api` é processada pelo processo **`workers`** — saldo
  `50.00 → 38.00`, `version 1 → 2`, dois lançamentos no ledger e o evento
  publicado na `integration-events.fifo` pelo outbox do outro processo;
- o log do `api` tem `0` consumers iniciados; o log do `workers` tem o
  consumer, o outbox e o reference resolver.

```sh
make e2e              # modo combinado (all)
make e2e SPLIT=1      # api + workers como processos separados
make e2e MULTI=1      # três apps combinados (concorrência distribuída)
```

### Kubernetes local (kind) — a segunda forma de subir o ambiente

O compose é a forma mais curta de subir tudo. A segunda forma é **Kubernetes
real**: um cluster `kind` de nó único, reconciliado por um **Helm chart**. Não é
uma reescrita — é o **mesmo binário, os mesmos manifests de config**, expressos
como Releases em vez de containers soltos.

A razão de existir: o compose prova que os processos cooperam; o cluster prova
que eles cooperam **quando o orquestrador decide onde e quando cada Pod nasce** —
com `readinessProbe` no lugar de `depends_on`, `Service` DNS no lugar dos nomes
de serviço do compose, e rollout/restart gerenciados. O wiring que o compose
escreve à mão (`depends_on` + healthcheck) vira contrato declarativo.

```sh
make kind-check          # confere kind/helm/kubectl e mostra o engine detectado
make kind-deploy         # cluster + imagem + chart, tudo em um passo
```

`make kind-deploy` encadeia quatro passos, cada um chamável sozinho:

| Alvo | Script | O que faz |
| :-- | :-- | :-- |
| `make kind-check` | `scripts/kind-check.sh` | **Preflight**: verifica `kind`/`helm`/`kubectl` e escolhe o engine (`docker` ou `podman`). Falha **antes** de criar nada, com o que instalar — em vez de estourar no meio do deploy |
| `make kind-up` | `scripts/kind-up.sh` | Cria o cluster (idempotente: reusa o existente; `RECREATE=1` força um limpo) |
| `make kind-image` | `scripts/kind-image.sh` | Builda a imagem local e a carrega no node |
| `make kind-deploy` | `scripts/kind-deploy.sh` | Instala/atualiza o release (`helm upgrade --install`) e espera os workloads |

**A imagem entra pelo node, não por um registry.** `kind load docker-image`
(o script usa `image-archive` quando o daemon está acessível mas `docker save`
não) streama o tarball direto para o containerd do node — não há registry nesta
máquina, e subir um só para alimentar um cluster de laptop seria uma peça móvel
a mais sem benefício. A tag é o **SHA curto do git** (com sufixo `-dirty` para
árvore suja), nunca `latest`: com `imagePullPolicy: IfNotPresent`, uma tag
reusada faria código novo rodar a imagem velha silenciosamente.

O chart já sabe alcançar suas dependências dentro do cluster (Service DNS — o
mesmo wiring que o compose expressa com `depends_on` + nomes de serviço), então
não há orquestração a fazer além de **ordem**: as dependências precisam existir
antes de as migrations da app rodarem.

#### Topologias e observabilidade

O chart cobre as mesmas topologias do compose, por flags de values:

```sh
make kind-deploy                          # app único (all)
make kind-split                           # api + workers (WALLET_MODE)
make kind-deploy OBSERVABILITY=1 SPLIT=1  # split + collector/tempo/loki/prom/grafana
```

| Flag | Efeito |
| :-- | :-- |
| `SPLIT=1` | Dois Deployments: `api` (pool pgx inteiro para HTTP, owner do schema) e `workers` (pool menor, sem migrations) — as réplicas de cada lado escalam na própria dimensão |
| `OBSERVABILITY=1` | Sobe o coletor OTLP, Tempo, Loki, Prometheus e Grafana como Pods do cluster |
| `RECREATE=1` | Recria o cluster do zero (logo, banco limpo) em vez de reusar |
| `VALUES=` / `SET=` | Arquivo de values extra / `--set` ad-hoc para o `helm` |

Os endpoints saem no host pelas `extraPortMappings` do `deploy/kubernetes/kind/cluster.yaml`,
**nos mesmos números do compose** — assim os exemplos de `curl` deste README
funcionam sem alteração contra qualquer um dos dois ambientes:

| Serviço | Host |
| :-- | :-- |
| API (`all`) | `http://localhost:8081` |
| API `api` / workers (split) | `http://localhost:8084` / `:8085` |
| Keycloak | `http://localhost:8080` |
| LocalStack | `http://localhost:4566` |
| Grafana / Prometheus (observabilidade) | `http://localhost:30300` / `:30909` |

O `helm` renderiza as combinações sem tocar em cluster nenhum:

```sh
make helm-lint           # lint + template em todas as topologias
make kind-status         # estado do release e dos workloads
WALLET_TEST_API_URL=http://localhost:8081 make e2e   # a suíte e2e, contra o cluster
make kind-down           # apaga o cluster inteiro (e os volumes)
```

#### Caminho GitOps (opcional): ArgoCD

Além do `helm upgrade`, o repositório traz o caminho **GitOps** ponta a ponta:
ArgoCD reconcilia o chart a partir de um **git in-cluster** (Gitea), dentro do
próprio cluster.

```sh
make kind-argocd         # sobe cluster + chart, instala ArgoCD + Gitea e o Application
make kind-argocd-seed    # re-semeia a pasta no git in-cluster (dispara um sync novo)
```

`scripts/kind-argocd-seed.sh` copia `deploy/kubernetes` para o Gitea e o
`Application` aponta para ele; re-rodar depois de uma edição produz uma revisão
nova para o ArgoCD sincronizar — o mesmo gesto de um `git push`, sem rede. A UI
do ArgoCD fica em `http://localhost:8000`.

> [!NOTE]
> O `helm` é a fonte de verdade dos manifests; o ArgoCD é um **consumidor** desse
> chart, não um caminho paralelo. Ambos instalam exatamente o mesmo release.

### Variáveis de ambiente

Todas documentadas em [`.env.example`](.env.example). As principais:

| Variável | Papel |
| :-- | :-- |
| `WALLET_DB_DSN` | Conexão PostgreSQL |
| `WALLET_SQS_ENDPOINT` | Endpoint SQS (LocalStack local / AWS em produção) |
| `WALLET_AUTH_ISSUER` | Issuer OIDC validado no `iss` do token |
| `WALLET_AUTH_JWKS_URL` | Override do JWKS (ambientes com hosts distintos) |
| `WALLET_WORKERS_ENABLED` | Liga/desliga consumer, outbox e reference resolver |
| `WALLET_MODE` | Papel do processo: `all` (padrão), `api` ou `workers` |
| `WALLET_REFERENCE_TTL` / `WALLET_REFERENCE_MAX_ATTEMPTS` | Política de referências pendentes |
| `WALLET_OTEL_ENABLED` | Liga o tracing (padrão `false`) |

### Migrations

Aplicadas automaticamente no start (`WALLET_MIGRATE_ON_START=true`). Manual:

```sh
go run ./cmd/walletd migrate up      # aplica pendentes
go run ./cmd/walletd migrate down    # reverte UMA migration (reversível, passo a passo)
go run ./cmd/walletd migrate status  # lista estado
```

---

## 📡 Observabilidade

Métricas, traces e logs, correlacionados por `trace_id`.

```mermaid
flowchart LR
    APP["walletd<br/>slog JSON + OTLP"]
    PROM["Prometheus<br/>:9090"]
    COLL["OTel Collector<br/>:4317 gRPC"]
    TEMPO["Tempo<br/>traces"]
    LOKI["Loki<br/>logs"]
    GRAF["Grafana :3000<br/>3 dashboards"]

    APP -->|/metrics scrape| PROM
    APP -->|OTLP| COLL
    COLL -->|spanmetrics + servicegraph| PROM
    COLL --> TEMPO
    COLL --> LOKI
    PROM --> GRAF
    TEMPO --> GRAF
    LOKI --> GRAF

    classDef app fill:#2ea04322,stroke:#2ea043,stroke-width:2px
    classDef obs fill:#425CC722,stroke:#425CC7
    class APP app
    class PROM,COLL,TEMPO,LOKI,GRAF obs
```

```sh
WALLET_OTEL_ENABLED=true docker compose --profile observability up --build -d
```

> [!NOTE]
> O tracing é **opt-in**: com `WALLET_OTEL_ENABLED=false` (padrão) o serviço sobe
> com um tracer no-op e o `/metrics` continua funcionando — a stack de
> observabilidade nunca é pré-requisito para rodar o serviço.

Os dashboards são **código** (`deploy/observability/grafana/dashboards/*.json`,
provisionados com `allowUiUpdates: false`): *Wallet — Overview*,
*Wallet — Integrity & Workers* e *Wallet — Traces*.

---

## 🔌 API

### Rotas de negócio (exigem Bearer token)

| Método | Rota | Quem pode | Descrição |
| :-- | :-- | :-- | :-- |
| `POST` | `/wallets` | `wallet-internal` | Abre carteira (`201 Created`); aceita `Idempotency-Key` |
| `GET` | `/wallets/{walletId}` | interno | Saldo e versão da carteira |
| `GET` | `/wallets/{walletId}/ledger` | interno | Lançamentos paginados (`?limit=`) |
| `POST` | `/wallets/{walletId}/reconciliation` | interno | Confere saldo × ledger |
| `POST` | `/wagering/transactions` | provedor | Submete BET/WIN/LOSS/REFUND/ROLLBACK |
| `GET` | `/wagering/transactions/{transactionId}` | interno | Lê operação por id interno |
| `GET` | `/providers/{providerId}/wagering/transactions/{externalTransactionId}` | provedor | Lê operação pelo id do provedor |

### Rotas públicas

| Método | Rota | Descrição |
| :-- | :-- | :-- |
| `GET` | `/health/live` | Liveness (processo vivo) |
| `GET` | `/health/ready` | Readiness (PostgreSQL **e** SQS acessíveis; `503` se algum cair) |
| `GET` | `/metrics` | Métricas Prometheus |

> [!NOTE]
> **Idempotência em `POST /wallets`.** O cabeçalho `Idempotency-Key` é
> opcional, mas quando presente torna a abertura 100% idempotente: a chave é
> gravada na **mesma transação** da carteira (tabela `wallet_open_requests`),
> de modo que repetir a mesma requisição devolve a carteira original com
> `"idempotentReplay": true` — sem criar uma segunda carteira nem duplicar o
> saldo de abertura. Reutilizar a chave com um corpo diferente devolve
> `409 IDEMPOTENCY_KEY_CONFLICT`. Sem chave, a unicidade
> `(playerId, currency)` continua sendo a guarda (`409 WALLET_ALREADY_EXISTS`).

> [!NOTE]
> Em `WALLET_MODE=workers` as rotas de **negócio** não são registradas: o
> processo responde `/health/*` e `/metrics` (probes e scraping não dependem do
> papel) e devolve `404` honesto para o resto, em vez de aceitar tráfego que
> não deveria atender.

### Identidades de teste (Keycloak)

O realm `wallet` é importado automaticamente
(`deploy/keycloak/realm-wallet.json`). Três clientes `client_credentials`:

| Cliente | Secret | Poder |
| :-- | :-- | :-- |
| `provider-a` | `provider-a-secret` | Operações **apenas** com `providerId=provider-a` |
| `provider-b` | `provider-b-secret` | Operações **apenas** com `providerId=provider-b` |
| `wallet-internal` | `internal-secret` | Serviço interno: abre/lê carteiras, vê tudo |

### Exemplos de chamada

```sh
TOKEN_INT=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=internal-secret | jq -r .access_token)
TOKEN_A=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret | jq -r .access_token)
API=http://localhost:8081

# 1. abrir carteira (interna) — Idempotency-Key é opcional, mas recomendado
curl -s -X POST $API/wallets -H "Authorization: Bearer $TOKEN_INT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: wallet-internal:open-player-42' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'

# 2. aposta (provider-a)
curl -s -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $TOKEN_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123",
       "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
       "walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp",
       "kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'

# 3. leitura, ledger e reconciliação
curl -s $API/wallets/<walletId> -H "Authorization: Bearer $TOKEN_INT"
curl -s "$API/wallets/<walletId>/ledger?limit=50" -H "Authorization: Bearer $TOKEN_INT"
curl -s -X POST $API/wallets/<walletId>/reconciliation -H "Authorization: Bearer $TOKEN_INT"

# 4. reversão (referência obrigatória)
curl -s -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $TOKEN_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:refund-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"refund-123",
       "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>",
       "roundId":"round-987","gameId":"fortune-chimp","kind":"REFUND",
       "money":{"amount":"25.00","currency":"BRL"},
       "referenceExternalTransactionId":"transaction-123"}'

# health / métricas
curl -s $API/health/live; curl -s $API/health/ready; curl -s $API/metrics | head
```

> [!WARNING]
> Uma reversão **deve** referenciar uma operação do **mesmo `roundId`**,
> do mesmo jogador, da mesma carteira e da mesma moeda — caso contrário a
> operação é rejeitada com `REFERENCE_MISMATCH`.

### Contrato da fila de entrada

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a", "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "...", "walletId": "...", "roundId": "round-987",
    "gameId": "fortune-chimp", "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

A identidade da mensagem é `sha256` do corpo. Eventos de saída são publicados em
`integration-events.fifo` com `MessageGroupId` = `aggregateId` e
`MessageDeduplicationId` = `eventId`.

### Códigos HTTP

| Situação | Status | Corpo |
| :-- | :-- | :-- |
| Entrada inválida (shape) | `400` | `{"error":{"code":"VALIDATION_FAILED",...}}` |
| Sem token / token inválido/expirado | `401` | `UNAUTHORIZED` |
| Autenticado sem permissão | `403` | `FORBIDDEN` |
| Carteira/transação inexistente | `404` | `WALLET_NOT_FOUND` / `TRANSACTION_NOT_FOUND` |
| Chave reutilizada; external id conflitante; carteira duplicada | `409` | `IDEMPOTENCY_KEY_CONFLICT` / `EXTERNAL_TRANSACTION_ID_CONFLICT` / `WALLET_ALREADY_EXISTS` |
| Saldo insuficiente; referência divergente | `422` | `INSUFFICIENT_FUNDS` / `REFERENCE_MISMATCH` / … |

---

## 🧪 Testes

### Unitários

```sh
go test ./...            # 71 testes (testify), table-driven
go test -race ./...      # com detector de corrida
go vet ./...             # + go vet -tags=integration ./...
gofmt -l .               # vazio = formatado
```

### ✅ Validação end-to-end (um comando)

Para quem vai avaliar: **um comando** sobe a stack, espera os healthchecks de
verdade, roda a suíte completa contra a infraestrutura real e derruba tudo no
final (mesmo em caso de falha):

```sh
make e2e                    # stack + suíte completa (race detector ligado)
make e2e MULTI=1            # inclui app-2/app-3 (concorrência distribuída)
make e2e ARGS="-run Journey"
make e2e KEEP=1             # deixa a stack no ar para inspeção manual
```

O runner (`scripts/e2e.sh`) funciona com `podman-compose` **ou** `docker
compose`; resolve o container pelo label do Compose, então não depende do nome
gerado, e imprime os logs do app se a suíte falhar.

### Testes de integração (containers reais — sem mock de infra)

```sh
docker compose up -d postgres keycloak localstack app   # dependências + app
make test-integration                                   # go test -tags=integration ./test/integration/...
```

Cobrem **29 testes**: migrations e constraints (ledger append-only, unicidades,
CHECKs), atomicidade financeira, cenário obrigatório de concorrência (100.00 e
duas apostas de 80.00, inclusive com **duas instâncias**), replays HTTP+SQS
cruzados, **idempotência de abertura de carteira** (`Idempotency-Key`), 
inbox/outbox, DLQ, resolução e expiração de referências (TTL), isolamento entre
provedores, rejeição de credenciais, ciclo de vida Fx e **recuperação de falha
de infraestrutura** (fail-fast de Postgres/SQS no boot e degradação honesta de
`/health/ready` a `503` quando uma dependência cai em runtime).

### Testes de jornada (histórias executáveis)

Além das asserções por endpoint, `journey_test.go` percorre o produto de ponta a
ponta como um operador o narraria, verificando o saldo a cada passo:

| Teste | O que prova |
| :-- | :-- |
| `TestJourneyFullPlayerLifecycle` | abrir 100.00 → BET 30.00 → WIN 12.50 → ROLLBACK do BET → saldo **112.50**, ledger com 4 lançamentos, reconciliação consistente e evento `WagerTransactionProcessed` na fila |
| `TestJourneyHTTPContractIsHonest` | cada rota documentada responde o code esperado (pega divergência entre README e código) |
| `TestJourneyHealthAndMetricsArePublic` | `/health/*` e `/metrics` são públicos; rota de negócio sem token → `401` |

### Testes de carga (k6)

`loadtest/` guarda os cenários de carga, escritos em **k6** — que roda a partir
da imagem oficial, sem instalar nada na máquina. O princípio é que carga sem
invariante não prova nada: cada cenário ataca uma garantia específica e falha
verificando **estado**, não status HTTP.

| Cenário | Garantia atacada |
| :-- | :-- |
| `smoke` | contrato inteiro (abertura, replay, conflito, 422, ledger, reconciliação) em segundos |
| `hot-wallet` | lock por linha no pior caso: uma carteira, N VUs; o `teardown` confere `saldo`, `version` e a soma do ledger |
| `idempotency` | as três cláusulas: replay, `409` na mesma chave com payload diferente, e o controle que pega dedup por hash |
| `mixed` | tráfego de produção com linhas adversariais — cada recusa no seu código |
| `distributed` | o lock **entre processos**: lê o saldo por todas as réplicas e falha se divergirem |
| `soak` | deriva sob carga sustentada, reconciliando cada carteira no fim |

O k6 mede a **API**; o que foi **commitado** é conferido por
`loadtest/sql/invariants.sql`, rodado direto no Postgres — cadeia do ledger,
saldo × soma, unicidade de chave de idempotência, `PROCESSED` sem lançamento.

```sh
make up                    # uma instância
make load-smoke            # contrato completo, segundos
make load                  # hot + idempotency + mixed

make up-multi              # três réplicas
make load-distributed      # lock entre processos

make load-verify           # 9 auditorias de integridade, ~1M entradas
```

> [!NOTE]
> Depois dos cenários o backlog do outbox fica na casa das centenas de milhares.
> É **esperado** neste teste: os cenários produzem a ~2.600 req/s enquanto o
> relay publica a ~200 eventos/s. Não é um bug — é a taxa do gerador superando a
> de publicação. O `load-verify` reporta o número como `info`, sem reprovar.

Detalhes de cada cenário, a calibração dos thresholds e o que **não** é coberto
estão em [`loadtest/README.md`](loadtest/README.md).

---

## 🔁 Integração contínua

O repo tem dois workflows em [`.github/workflows/`](.github/workflows). O
princípio é que **o CI roda exatamente o que o README manda um humano rodar** —
não existe um caminho só-de-CI que possa divergir da documentação.

### `ci.yml` — qualidade e as duas topologias

| Job | O que roda | Por que existe |
| :-- | :-- | :-- |
| `quality` | `gofmt -l`, `go vet` (com e sem `-tags=integration`), `go test -race ./...`, coverage como artefato | Feedback rápido e hermético, sem containers. O `vet` com tag pega erro de compilação nos testes de integração **antes** do job caro |
| `e2e` | `./scripts/e2e.sh` — compose real (Postgres + Keycloak + LocalStack) + suíte completa | É o job que **prova** as afirmações do desafio em vez de afirmá-las; 25 testes com race detector |
| `e2e-split` | `SPLIT=1 ./scripts/e2e.sh` | Exercita `WALLET_MODE=api` + `WALLET_MODE=workers` como processos separados — se passar, o argumento de topologia do README está verificado a cada push |
| `load` | `make load-smoke`, `make load-hot`, `make load-verify` | Sobe a stack, roda o contrato no k6 e depois audita o que foi **commitado** — fecha a lacuna entre "respondeu certo" e "está consistente" |
| `image` | build da imagem, uid ≠ 0, `walletd version`, boot com `WALLET_MODE` inválido | Invariantes de segurança e de operação viram **falha de build**, não comentário de revisão |

O job `image` verifica três coisas que costumam ser só prometidas:

- a imagem roda como **não-root** (`uid 10001`), então remover o `USER app` quebra o CI;
- `walletd version` funciona **sem banco e sem config** (é um probe de ops);
- o processo **recusa subir** com `WALLET_MODE=banana`, e a mensagem cita a
  variável — fail-fast é testado no caminho real de boot (sem subcomando,
  onde o `config.Load` roda), não num atalho que passaria por acidente.

> [!NOTE]
> Cada push novo na mesma ref **cancela** a execução anterior
> (`concurrency.cancel-in-progress`): resultado de um run superado não tem
> valor, e minutos de CI são recurso.

### `release.yml` — imagem e release

Disparado por tag semver (`v1.2.3`) ou manualmente. Publica no GHCR uma imagem
**multi-arquitetura** (`linux/amd64` + `linux/arm64`), com SBOM e provenance, e
cria o GitHub Release com as instruções dos modos `api`/`workers`.

```sh
git tag v1.0.0 && git push origin v1.0.0
```

### Rodando o que o CI roda, na sua máquina

```sh
gofmt -l . && go vet ./... && go vet -tags=integration ./...   # job quality
go test -race -count=1 ./...
make e2e          # job e2e
make e2e SPLIT=1  # job e2e-split
UP_ONLY=1 make e2e && make load-smoke && make load-hot && make load-verify  # job load
```

---

## 💥 Simulações de falha

| Cenário | Como provocar | O que observar |
| :-- | :-- | :-- |
| 💳 **Concorrência por carteira** | duas apostas de 80.00 numa carteira de 100.00 | exatamente uma passa; a outra `INSUFFICIENT_FUNDS`; saldo 20.00 |
| ⚡ **Concorrência entre réplicas** | `docker compose --profile multi up`; uma aposta por instância | o lock row-level serializa sem sobrescrever |
| 🔁 **Replay HTTP** | repetir a mesma `Idempotency-Key` | resposta idêntica, `idempotentReplay: true`, sem novo lançamento |
| 🔑 **Replay de abertura** | repetir `POST /wallets` com a mesma `Idempotency-Key` | mesma carteira, `idempotentReplay: true`, saldo de abertura não duplica |
| 📨 **Replay SQS** | reenviar a mesma mensagem | inbox deduplica; saldo não muda |
| ☠️ **Consumidor morre pós-commit, pré-delete** | `docker compose kill app` durante carga | inbox absorve a reentrega; saldo não duplica |
| 📤 **Publicador morre entre commit e publish** | `docker compose pause app` antes do worker publicar; depois `unpause` | outbox publica com o mesmo `eventId` |
| 🥊 **Dois publishers na outbox** | duas instâncias com fila de eventos vazia | `FOR UPDATE SKIP LOCKED` + dedup por `eventId` |
| ⏳ **Referência antes da transação** | enviar `REFUND` antes do `BET` | `PENDING_REFERENCE` → resolução automática (ou `REFERENCE_NOT_FOUND` após TTL) |
| 🔄 **Reinício da aplicação** | `docker compose restart app` | idempotência e pendências sobrevivem (persistidas no Postgres) |
| 🛑 **Postgres fora no boot** | apontar `WALLET_DB_DSN` para uma porta morta | a instância **falha rápido** (não sobe contra um banco inalcançável) |
| 📵 **SQS fora no boot** | apontar `WALLET_SQS_ENDPOINT` para uma porta morta | a instância **falha rápido** ao resolver as filas |
| 🩺 **Dependência cai em runtime** | derrubar o SQS com a app no ar | `/health/ready` vira `503`; `/health/live` segue `200` (processo vivo, sem tráfego) |

---

## 🧠 Decisões técnicas

Detalhadas em [`ARCHITECTURE.md`](ARCHITECTURE.md) — dinheiro, transações,
idempotência, locks, referências pendentes, reversões, inbox/outbox,
autenticação, Fx e shutdown.

---

<div align="center">

**[⬆ Voltar ao topo](#-wallet--desafio-backend)**

</div>
