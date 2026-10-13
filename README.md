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
[![Testes unitários](https://img.shields.io/badge/unit%20tests-236%20passing-2ea043)](#-testes)
[![Testes de integração](https://img.shields.io/badge/integration%20tests-124%20passing-2ea043)](#-testes)
[![Cobertura](https://img.shields.io/badge/coverage-91%25-2ea043)](#-testes)
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
  - [Testes de integração (containers reais)](#testes-de-integração-containers-reais--sem-mock-de-infra)
  - [Testes de carga (k6)](#testes-de-carga-k6)
- [Integração contínua](#-integração-contínua)
- [Verificação com race detector (réplicas)](#-verificação-com-race-detector-réplicas)
- [Reconciliação contínua](#-reconciliação-contínua)
- [Banco: pooling e auditoria](#-banco-pooling-e-auditoria)
  - [PgBouncer — pool de conexões](#pgbouncer--pool-de-conexões)
  - [pgAudit — auditoria no servidor](#pgaudit--auditoria-no-servidor)
- [Políticas: Keycloak e SQS](#-políticas-keycloak-e-sqs)
  - [Keycloak — identidade e audience](#keycloak--identidade-e-audience)
  - [SQS — políticas de fila](#sqs--políticas-de-fila)itoria-no-servidor)
- [Simulações de falha](#-simulações-de-falha)
- [Decisões técnicas](#-decisões-técnicas)
  - [Partidas dobradas (diferencial opcional)](#partidas-dobradas-diferencial-opcional)

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
| 🛡️ **Identidade mínima** | Token de máquina com escopo enxuto e `aud` exigida; filas com redrive e long polling provados |

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
        REC["messaging/reconcile<br/>varredura de reconciliação"]
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
    REC --> SVC
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
    class HTTP,CONS,REL,REF,REC,PG,SQS,AUTH,MET adapter
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
| **Pooling** | PgBouncer em `pool_mode = transaction` (profile `pooled`) |
| **Auditoria no banco** | pgAudit (`pgaudit.log = 'write,ddl,role'`) na imagem `deploy/pgaudit` (profile `audit`) |
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
  keycloak/             # realm-wallet.json (import automático) + README das policies
  localstack/           # init: filas FIFO + README das políticas de fila
  pgbouncer/            # pooler: pgbouncer.ini (transaction mode) + userlist
  pgaudit/              # Dockerfile (Postgres 17 + extensão) + pgaudit.conf
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
| 🔀 **PgBouncer** | localhost:6433 | só com `--profile pooled` (app em `:8086`) |
| 🛡️ **Postgres + pgAudit** | localhost:5434 | só com `--profile audit` |

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

Para a variante que roda as réplicas **sob o race detector** — e prova que o
silêncio do detector é significativo — veja
[Verificação com race detector](#-verificação-com-race-detector-réplicas).

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
| `WALLET_AUTH_AUDIENCE` | Audience **exigida** no `aud` do token (`wallet-api`). Vazio desliga a checagem; o compose define `wallet-api` — ver [Políticas: Keycloak e SQS](#-políticas-keycloak-e-sqs) |
| `WALLET_WORKERS_ENABLED` | Liga/desliga consumer, outbox, reference resolver e reconciliação |
| `WALLET_MODE` | Papel do processo: `all` (padrão), `api` ou `workers` |
| `WALLET_REFERENCE_TTL` / `WALLET_REFERENCE_MAX_ATTEMPTS` | Política de referências pendentes |
| `WALLET_RECONCILE_INTERVAL` / `WALLET_RECONCILE_BATCH` | Cadência e tamanho do lote da varredura de reconciliação (padrão `5m` / `100`) |
| `WALLET_OTEL_ENABLED` | Liga o tracing (padrão `false`) |
| `WALLET_DB_MAX_CONNS` | Teto de conexões do pool **por processo** (padrão `20`) — dimensionar contra `max_connections` do Postgres, ou deixar o PgBouncer absorver |

> **PgBouncer e pgAudit** não são variáveis do app: eles mudam o *caminho até o
> banco*, não a configuração do serviço. São profiles do compose (`pooled`,
> `audit`) — ver [Banco: pooling e auditoria](#-banco-pooling-e-auditoria).

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

| Cliente | Secret | Escopo no token | Poder |
| :-- | :-- | :-- | :-- |
| `provider-a` | `provider-a-secret` | `wallet:provider` | Operações **apenas** com `providerId=provider-a` |
| `provider-b` | `provider-b-secret` | `wallet:provider` | Operações **apenas** com `providerId=provider-b` |
| `wallet-internal` | `internal-secret` | `wallet:internal` | Serviço interno: abre/lê carteiras, vê tudo |

Os escopos são enxutos de propósito — `email`, `profile` e roles de realm
**não** entram num token de máquina. Ver
[Políticas: Keycloak e SQS](#-políticas-keycloak-e-sqs).

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
go test ./...            # 236 testes (testify), table-driven
go test -race ./...      # com detector de corrida
go vet ./...             # + go vet -tags=integration ./...
gofmt -l .               # vazio = formatado
```

Cobertura combinada (unit + integração, com a stack no ar): **91,1%** —
`1979/2172` statements, medidas com `-coverpkg=./cmd/...,./internal/...` sobre os
dois conjuntos de pacotes. Os badges de teste acima saem destas duas contagens.

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

Cobrem **124 testes** distribuídos por `test/integration/`, `internal/postgres/`
e `internal/transport/http/`: migrations e constraints (ledger append-only,
unicidades, CHECKs), atomicidade financeira, cenário obrigatório de concorrência
(100.00 e duas apostas de 80.00, inclusive com **duas instâncias**), replays
HTTP+SQS cruzados, **idempotência de abertura de carteira** (`Idempotency-Key`),
inbox/outbox, DLQ, resolução e expiração de referências (TTL), isolamento entre
provedores, rejeição de credenciais, ciclo de vida Fx e **recuperação de falha
de infraestrutura** (fail-fast de Postgres/SQS no boot e degradação honesta de
`/health/ready` a `503` quando uma dependência cai em runtime).

#### Interrupção do consumidor entre o commit e o ack

O cenário 5 do enunciado — *"interrompa um consumidor depois do commit e antes da
remoção da mensagem"* — é coberto por `kill_after_commit_test.go`. Ele não depende
de `docker kill` nem de timing: um `commitGateTxManager` envolve o `Store` real e,
após o **primeiro commit bem-sucedido**, bloqueia a transação até o teste cancelar
o contexto. A instância #1 morre exatamente no ponto exigido (pós-commit,
pré-`DeleteMessage`); a reentrega vai para uma instância #2 com store real, que
precisa **reconhecer a duplicata pelo inbox durável**, ackar sem debitar de novo,
não consumir uma segunda linha de inbox e não mandar a mensagem para a DLQ.

A prova de que o teste morde o comportamento, e não a implementação: trocar
`if tag.RowsAffected() == 1` por `if true` em `inbox_repo.go` (aceitar toda
reentrega como nova) faz o teste falhar — o inbox deixaria de deduplicar.

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

## 🧪 Verificação com race detector (réplicas)

O repo já prova duas coisas **separadamente**: `go test -race` pega corridas de
memória *dentro do processo de teste*, e o `TestMultiInstanceConcurrency` (mais
o cenário k6 `distributed`) prova que o lock de linha impede **três réplicas** de
perder um update. Falta o vão entre as duas: rodar os **próprios binários do
serviço** sob `-race`, **três de uma vez**, batendo na mesma carteira.

```sh
make race-replicas        # canário + 3 réplicas + carga + verificação
```

O script afirma **duas propriedades distintas**, porque nenhuma implica a outra:

| # | Propriedade | O que ela significa |
| :- | :-- | :-- |
| 1 | **Interna** | cada réplica, sob tráfego concorrente real (handlers HTTP + relay do outbox + consumer SQS rodando juntos), **não reporta data race** nas suas próprias goroutines |
| 2 | **Externa** | as três réplicas **concordam no saldo** — o lock vale *entre processos*, não só dentro de um |

### O canário: por que um "silêncio" vale como evidência

Um detector de race que **não vê nada** e um detector que **não está rodando**
produzem a mesma saída: nenhuma. Então um "rodei `-race` e não apareceu aviso"
só tem valor se antes for possível **fazer o detector gritar**.

A Fase A do script faz exatamente isso: roda a mesma imagem com
`WALLET_RACE_CANARY=1`, que provoca uma corrida **real** entre duas goroutines
numa variável (`cmd/walletd/race_canary.go`), e exige ver `WARNING: DATA RACE`.
Se não vir, **aborta** — não reporta um verde que não significaria nada. A Fase A
também valida a direção oposta (o canário desligado tem que ser silencioso),
o que fecha o caso de uma imagem que imprime race incondicionalmente.

Esse passo é, além disso, o que **valida o próprio `Dockerfile.race`**: se cgo ou
glibc estivessem errados, o canário falharia ali.

> [!NOTE]
> O `Dockerfile.race` **não** é a imagem de produção. `-race` exige cgo, e cgo
exige glibc — o `Dockerfile` normal compila `CGO_ENABLED=0` sobre alpine (musl)
e um binário instrumentado **não sobe** ali. Por isso a verificação tem imagem
própria (Debian), e ela **não** entra no artefato entregue: a afirmação é sobre a
*verificação*, não sobre o binário.

Saída real de uma execução (`make race-replicas`):

```
==> Phase A: canary — make the race detector WARN on purpose
    ok: detector fired on the canary
    ok: detector is silent when the canary is off
==> Phase B: booting 3 race-instrumented replica(s)
    replica 1 ready on :9081 ...
==> Phase C: concurrent traffic (16 workers x 40 iters) on one wallet
    distributed OK — 3 instances agree on 1640.00 (v641), 641 ledger entries, reconciliation consistent
==> Checking every replica's log for data races
    wallet_race_1    DATA RACE lines: 0
    wallet_race_2    DATA RACE lines: 0
    wallet_race_3    DATA RACE lines: 0
==> race-replicas: PASS — detector proven live, replicas race-free, ledger consistent
```

---

## 🧮 Reconciliação contínua

A reconciliação sob demanda (`POST /wallets/{id}/reconciliation`) responde
"esta carteira está consistente?" — mas só se alguém perguntar **por aquela
carteira**. Uma carteira cujo saldo foi corrompido por um deploy ruim ou um
`UPDATE` manual fica indetectada até um cliente, por acaso, reconciliá-la.

O **worker de reconciliação** fecha essa janela: a cada
`WALLET_RECONCILE_INTERVAL` ele varre **todas** as carteiras em lotes keyset e
compara cada saldo armazenado com a soma do seu ledger. Uma divergência que
exista enquanto ninguém olhava passa a ser um fato do sistema em execução, não
uma pergunta que depende de um cliente.

```sh
# cadência curta para observar o comportamento localmente
WALLET_RECONCILE_INTERVAL=2s docker compose up -d --force-recreate app
curl -s localhost:8081/metrics | grep ^wallet_reconciliation
```

**Ele é somente-leitura — e isso é uma decisão, não uma omissão.** O worker
relata e instrumenta; nunca corrige. Auto-correção silenciosa seria um segundo
caminho de escrita de saldo, e o ledger é a única fonte de verdade: uma
divergência é um fato a ser exposto, não um número a ser maquiado. A varredura
reusa o **mesmo caso de uso** que a rota HTTP chama (`WalletService.Reconcile`),
então não há uma segunda implementação da aritmética para divergir da primeira.

### Varredura em lotes, não `OFFSET`

As carteiras são paginadas por **keyset** (`WHERE id > $last ORDER BY id LIMIT
$n`), não por `OFFSET`. `id` é uma ordem total que sobrevive a inserções e
remoções concorrentes durante a passagem: nenhuma carteira é pulada ou visitada
duas vezes, e a memória fica limitada ao tamanho do lote — não importa se a
tabela tem mil ou milhões de linhas.

Uma carteira removida entre a listagem e a checagem (corrida de ciclo de vida) é
**ignorada**, não contada: `Reconcile` mapeia o `ErrRepoNotFound` do repositório
para `WALLET_NOT_FOUND`, e é esse código — não o sentinela do repositório — que
identifica o caso benigno.

### Métricas

Três séries, no mesmo registry das demais:

| Métrica | Significado |
| :-- | :-- |
| `wallet_reconciliation_sweeps_total` | passagens completas — distingue "sem divergência" de "**nenhuma varredura rodou**" |
| `wallet_reconciliation_checked_total` | carteiras comparadas contra o ledger |
| `wallet_reconciliation_divergences_total` | carteiras divergentes — **compartilhada** com a rota sob demanda |

> [!IMPORTANT]
> `divergences_total` é incrementada em **um só lugar**: o caso de uso
> `WalletService.Reconcile`, que a rota HTTP e a varredura compartilham. O worker
> **não** soma de novo — isso contaria cada divergência duas vezes (uma pelo
> `Reconcile`, outra pelo resumo da varredura). O setup é verificado contando:
> com uma divergência plantada e N passagens, a série lê exatamente N.

### Saída real (divergência plantada por `UPDATE` direto)

```
wallet_reconciliation_sweeps_total 5
wallet_reconciliation_checked_total 870          # 5 passagens × 174 carteiras
wallet_reconciliation_divergences_total 5        # 1 plantada, vista uma vez por passagem
```

```json
{"level":"ERROR","msg":"reconciliation divergence","walletId":"01a1363e-…","stored":"213.45","calculated":"90.00","difference":"123.45"}
{"level":"ERROR","msg":"reconciliation sweep found divergences","worker":"reconcile","checked":174,"divergences":1}
```

O teste de integração `TestContinuousReconciliationFindsPlantedDivergence` roda o
worker contra o **banco real**, planta a divergência com um `UPDATE` na tabela
`wallets` (o ledger fica intacto — o trigger append-only proíbe editá-lo), e
verifica tanto a detecção quanto o controle positivo: reparado o saldo, a mesma
carteira reconcilia limpa.

---

## 🗄️ Banco: pooling e auditoria

Duas peças que vivem **fora** do binário — e por isso mesmo provam coisas que o
código sozinho não prova. Ambas sob profiles do compose: o ambiente do dia a dia
(`docker compose up`) continua falando direto com o Postgres, sem pooler e sem
extensão.

| Peça | Profile | O que muda | O que prova |
| :-- | :-- | :-- | :-- |
| **PgBouncer** | `pooled` | acesso ao banco via `pgbouncer:6432` | N clientes colapsam em M conexões reais |
| **pgAudit** | `audit` | `postgres-audit` com a extensão carregada | todo WRITE/DDL deixa rastro no servidor |

```sh
make up-pooled                             # app :8086 via pooler
make up-audit                              # postgres :5434 com pgAudit
```

### PgBouncer — pool de conexões

O Postgres aceita `max_connections = 100`. Sem pooler, cada processo abre até
`WALLET_DB_MAX_CONNS` conexões **permanentes**: três réplicas `multi` (3 × 20)
mais o par `split` (15 + 5) chegam perto do teto antes de qualquer pico. Com o
PgBouncer em `pool_mode = transaction`, todas as conexões de cliente compartilham
`default_pool_size` (10) conexões reais no servidor.

**O detalhe que quase todo mundo erra.** `transaction` mode devolve a conexão de
servidor ao pool no fim da transação — não no fim da sessão. Isso quebra prepared
statements nomeados de drivers ingênuos (`ERROR: prepared statement
"stmtcache_…" already exists`), porque a conexão troca de dono entre execuções.
O `session` mode seria a saída "segura" e é justamente por isso que não serve:
uma conexão fica amarrada ao cliente a sessão inteira e o pooler vira um proxy
caro sem multiplexação. `statement` mode proíbe transações multi-statement — e o
wallet usa `WithinTx` para gravar saldo + ledger + inbox/outbox atomicamente,
então também não dá.

O **pgx v5 detecta o PgBouncer no handshake** e cai para o protocolo simples
sozinho, re-parseando cada statement. Nada no código do wallet precisou mudar — e
isto não é uma afirmação de fé:

```
$ WALLET_TEST_DB_DSN=postgres://wallet:wallet@localhost:6433/wallet?sslmode=disable \
    go test -tags=integration ./test/integration/ -count=1
ok      github.com/chalkan3/backend-challenge-go/test/integration     36.994s
```

A suíte de integração **completa** — 40+ sessões concorrentes, transações
`WithinTx`, idempotência sob corrida — passa idêntica pelo pooler. O dia em que
um driver trocar por um sem essa detecção, `default_query_exec_mode=simple_protocol`
no DSN resolve; o README registra o porquê para o próximo leitor não achar que
foi sorte.

**Observabilidade do pool.** `SHOW POOLS` e `SHOW STATS` expõem o multiplexing
(`admin_users`/`stats_users` são obrigatórios, senão o comando nega com
`FATAL: not allowed`):

```
$ psql "postgres://wallet:wallet@localhost:6433/pgbouncer" -c 'SHOW POOLS'
 database |   user    | cl_active | sv_idle |  pool_mode
----------+-----------+-----------+---------+-------------
 wallet   | wallet    |         2 |       2 | transaction

$ psql "postgres://wallet:wallet@localhost:6433/pgbouncer" -c 'SHOW STATS'
 database | total_server_assignment_count | total_xact_count | total_query_count
----------+-------------------------------+------------------+-------------------
 wallet   |                           130 |              130 |               178
```

`cl_active` (conexões de cliente) contra `sv_idle`/`total_server_assignment_count`
(conexões de servidor) é a leitura direta do ganho: 2 clientes, 2 backends, com
130 atribuições de servidor — a conexão sendo **reaproveitada**, não reaberta.

### pgAudit — auditoria no servidor

O ledger é append-only por três mecanismos que vivem **dentro** da aplicação: a
ausência de `UPDATE`/`DELETE` nos repositórios, os triggers `ledger_no_update`/
`ledger_no_delete` na tabela, e as auditorias do `invariants.sql`. Todos são
afirmações sobre *este binário*. Nenhum responde "quem tentou mexer no ledger com
um `psql` aberto?".

O pgAudit responde. Com `pgaudit.log = 'write, ddl, role'`, todo statement que
escreve gera uma linha imutável no log do **servidor**, com usuário, database,
statement e relação afetada. Isto foi verificado plantando os dois crimes:

```
$ docker compose --profile audit up -d
$ psql "postgres://wallet:wallet@localhost:5434/wallet" \
    -c 'UPDATE wallet_ledger_entries SET amount_minor = 999999;'
UPDATE 0                          # ← o trigger BARROU a mutação
$ psql "postgres://wallet:wallet@localhost:5434/wallet" \
    -c 'DELETE FROM wallet_ledger_entries;'
DELETE 0                          # ← idem

$ docker compose --profile audit logs postgres-audit | grep 'AUDIT.*WRITE'
… LOG:  AUDIT: SESSION,1,1,WRITE,UPDATE,TABLE,public.wallet_ledger_entries,UPDATE wallet_ledger_entries SET amount_minor = 999999,<not logged>
… LOG:  AUDIT: SESSION,1,1,WRITE,DELETE,TABLE,public.wallet_ledger_entries,DELETE FROM wallet_ledger_entries,<not logged>
```

O ponto não é que o trigger falhou — ele **funcionou** (`UPDATE 0`). O ponto é
que a **tentativa** ficou registrada. Defesa em profundidade: o trigger impede a
mutação, o pgAudit grava quem tentou, quando e contra qual tabela.

`pgaudit.log_relation = on` é o que faz a linha ser legível: sem ele, o log
mostraria o SQL mas não a tabela de forma estruturada — e um `UPDATE` em
`wallets` e um em `wallet_ledger_entries` seriam indistinguíveis fora do texto do
statement. `pgaudit.log = 'read'` fica **desligado** de propósito: o serviço faz
um volume enorme de `SELECT` (leitura de saldo, listagem do ledger) e auditar
leitura inundaria o log. O que interessa para a integridade é *escrita*.

Duas metades, e ambas precisam existir:

1. **O `.so` carregado no boot** — `shared_preload_libraries = 'pgaudit'` (a
   extensão registra hooks do executor; não dá para habilitar a quente). Isto
   mora na imagem: `deploy/pgaudit/Dockerfile`, base Debian com
   `apt install postgresql-17-pgaudit` (o PGDG publica o binário que casa com o
   major 17). O `postgres:17-alpine` do dia a dia **não** traz o pgAudit — nem o
   `.so` nem o control file, verificado.
2. **A extensão no catálogo** — `CREATE EXTENSION pgaudit`, na migration
   `00005_pgaudit.sql`. Ela é **condicional** (`IF EXISTS … pg_available_extensions`):
   num banco sem a extensão disponível (Alpine, CI, suíte de integração) a
   migration vira no-op e a subida segue. Auditoria é um *reforço* da
   integridade, não um pré-requisito de boot — a migration aplica em ambos os
   bancos, com e sem a extensão.

---

## 🛡️ Políticas: Keycloak e SQS

Dois lugares onde **política** significa coisas diferentes, e vale não
confundir os dois:

| | **Keycloak** | **SQS** |
| :-- | :-- | :-- |
| Política sobre | **quem é o principal e o que ele pode afirmar** | **como a fila se comporta sob falha** |
| Onde se declara | `deploy/keycloak/realm-wallet.json` | `deploy/localstack/init/10-queues.py` |
| Prova | teste de integração lê o Admin API **e** decodifica o token | teste lê os atributos **e** observa a mensagem chegar na DLQ |

### Keycloak — identidade e audience

Medido no token real de um provedor **antes** destas políticas: `aud: "account"`,
`scope: "profile email"`, `realm_access.roles`, `resource_access`, `email_verified`.
Um client de **máquina** recebia escopo e claims de **usuário**, e o serviço
aceitava — porque `WALLET_AUTH_AUDIENCE` estava vazio, ou seja, um token emitido
para outro público no mesmo realm também autenticaria.

Depois:

```json
{ "aud": "wallet-api", "scope": "wallet:audience wallet:provider",
  "azp": "provider-a", "providerId": "provider-a" }
```

Três políticas, e a razão de cada uma:

- **Audience** — um client scope `wallet:audience` injeta `aud=wallet-api`, e o
  serviço **exige** essa audience (`WALLET_AUTH_AUDIENCE: wallet-api` no compose).
  Emitir sem exigir deixaria o furo aberto; a exigência é o que transforma a
  emissão em política.
- **Escopo enxuto** — `fullScopeAllowed: false` e client scopes dedicados
  (`wallet:provider`, `wallet:internal`). `profile`, `email`, `roles`,
  `web-origins` e `acr`, que o Keycloak atribui por padrão a *todo* client,
  deixam de entrar num token de máquina. O escopo **não** é portão de
  autorização — as claims (`providerId`, `wallet:internal`) são assinadas e já
  infalsificáveis; exigir escopo adicionaria um modo de falha sem adicionar
  segurança. O escopo é higiene e observabilidade.
- **Token/security policies do realm** — `revokeRefreshToken`, `bruteForceProtected`,
  `eventsEnabled`, `adminEventsEnabled`.

> [!WARNING]
> **`--import-realm` não reimporta um realm existente.** O Keycloak registra no
> boot `Strategy: IGNORE_EXISTING`; se o realm `wallet` já existe no volume, um
> `realm-wallet.json` editado **não** é aplicado. Ambientes antigos precisam
> recriar o realm. É por isso que a prova é um teste que lê as policies de volta:
> ele falha alto contra um realm obsoleto, em vez de testar o realm errado.

### SQS — políticas de fila

O que **de fato** protege a mensageria aqui: `RedrivePolicy` (`maxReceiveCount:
5`, mensagem envenenada vai para a DLQ), `VisibilityTimeout: 60` (precisa exceder
o orçamento do consumer, ou uma mensagem lenta é reentregue *em voo*),
`ReceiveMessageWaitTimeSeconds: 20` (long polling), dedup explícita por id de
evento em `integration-events.fifo`, e retenção de 14 dias na DLQ (mensagem
quarentenada é evidência de um bug).

> [!NOTE]
> **Resource policy não é aplicada no LocalStack community — medido.** Um
> `Deny *` em `sqs:*` foi gravado numa fila e o `SendMessage` passou mesmo
> assim. `iam`/`sts` estão `disabled` no broker. Provisionar uma resource policy
> aqui seria config decorativa, então **não há nenhuma** — o teste
> `TestSQSResourcePolicyIsAcceptedButNotEnforced` fixa esse limite e começará a
> falhar quando o LocalStack passar a aplicá-las, que é quando o provisionamento
> deve ganhar uma. Em AWS real: resource policy restringindo o publisher, IAM
> por serviço no lugar das credenciais estáticas, KMS gerenciado pelo cliente.

A medição também revelou um bug no próprio provisioning: `create_queue` é
rejeitado com `QueueAlreadyExists` quando os atributos diferem, deixando os
antigos no lugar — um script que só cria é bootstrap de uma vez só, não
provisionamento. `ensure_queue` agora captura esse erro e **reconcilia** via
`set_queue_attributes`, então re-rodar é seguro.

### Rodando as provas

```sh
export WALLET_TEST_KEYCLOAK=http://localhost:8080 WALLET_TEST_SQS_ENDPOINT=http://localhost:4566
go test -tags=integration ./test/integration/ -run 'TestKeycloak|TestSQS.*Polic|TestSQSRedrive|TestSQSResource' -v
```

Detalhes completos: [`deploy/keycloak/README.md`](deploy/keycloak/README.md) e
[`deploy/localstack/README.md`](deploy/localstack/README.md).

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

### Partidas dobradas (diferencial opcional)

O ledger existente é **unilateral**: por movimento, garante
`balance_after = balance_before ± amount`, e a soma do ledger é conferida contra
o saldo por `loadtest/sql/invariants.sql` (auditoria #4). O que ele *não* expressa
é a segunda perna: **todo débito é crédito em algum lugar**, de modo que o
sistema conserve valor como um todo.

`internal/domain/doubleentry.go` modela essa segunda perna como **núcleo de
domínio puro** (sem I/O, sem dependência de infraestrutura):

| Conceito | Papel |
| :-- | :-- |
| `Account` | balde de valor identificado por `(kind, ref)` — `PLAYER_WALLET`, `HOUSE`, `PROVIDER_SETTLEMENT`, `REVENUE`; `HOUSE` e `REVENUE` são *singleton* sem `ref` |
| `Posting` | uma perna imutável: direction (`DEBIT`/`CREDIT`) + `Money` |
| `JournalEntry` | as pernas de uma transação; **rejeita entrada desbalanceada** na construção e ordena as pernas por conta (forma canônica) |

Cada movimento vira duas pernas — `BET` debita a carteira do jogador e credita a
casa; `WIN` faz o inverso — e `PlayerDelta(walletID)` recupera a variação do
jogador somando as pernas assinadas.

**E está gravado, não só modelado.** A migration `00004_postings.sql` cria a
tabela `postings`, e o serviço escreve a segunda perna **dentro da mesma
transação SQL** do saldo, em dois pontos: `finishProcessed` (BET/WIN/REFUND/
ROLLBACK) e a abertura de carteira. Um movimento que não consiga registrar sua
contraparte não é aplicado — as duas escritas commitam juntas ou não commitam.

O que a tabela garante, e onde é imposto:

| Garantia | Onde |
| :-- | :-- |
| O journal **não pode ser editado** | triggers `posting_no_update`/`posting_no_delete` recusam `UPDATE`/`DELETE` — journal que se edita não é evidência |
| A perna do jogador **não pode divergir do ledger** | trigger `posting_matches_ledger_check`: um posting `PLAYER_WALLET` sem a linha de ledger correspondente (mesma carteira, direção e valor) é rejeitado no `INSERT` |
| Cada transação **fecha** | auditoria #11: agrupa por transação e soma créditos − débitos; não-zero é uma transação que criou ou destruiu valor |
| Nenhum movimento **sem journal** | auditoria #11b: movimento processado com linha de ledger mas sem posting |
| As pernas **concordam com o histórico** | auditoria #11c: pega divergência introduzida por restore ou migração que passou por fora do trigger |

> [!NOTE]
> A perna do jogador no journal **é** a linha do ledger: mesmo
> `(transaction_id, direction, amount_minor)`. A tabela `postings` é um
> superconjunto — carrega também a contraparte —, então o ledger continua sendo o
> caminho rápido para a aritmética de saldo e o journal passa a ser o caminho de
> auditoria, que responde "e quem estava do outro lado?".

O valor não está em ter a tabela; está em a invariante ser **verificável** depois
do tráfego:

```
AUDIT|11|every journal entry balances|0
AUDIT|11b|every processed movement has a journal entry|0
AUDIT|11c|player postings match their ledger rows|0
```

E as auditorias **mordem**: contra um desbalanço plantado de propósito o detector
acusa (`CONTROL|11|planted imbalance detected (want 1)|1`) e uma perna divergente
é sinalizada (`CONTROL|11c|divergent player leg flagged (want 1)|1`) — auditoria
que nunca falha é indistinguível de auditoria que não roda. Isso foi medido, não
presumido.

Dois níveis de teste sustentam a ligação:

| Nível | O que prova |
| :-- | :-- |
| Unitário (`internal/service/doubleentry_wiring_test.go`) | BET debita o jogador e credita a casa; WIN é o espelho; OPENING nasce com journal; LOSS (valor zero) não escreve nada |
| Integração (`test/integration/journal_test.go`) | contra Postgres: a entrada existe e fecha, a perna do jogador bate com o ledger, o trigger **recusa** uma perna divergente, e um BET rejeitado não deixa journal |

Os testes de domínio originais seguem valendo, inclusive a prova por mutação:
forçar `Balanced()` a `return true` e trocar as pernas de `BET`
(`player, house` → `player, player`) fazem a suíte falhar — os testes mordem a
aritmética, não apenas a API.
---

<div align="center">

**[⬆ Voltar ao topo](#-wallet--desafio-backend)**

</div>
