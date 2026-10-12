# Wallet — Desafio Backend (Go + go-kit + Uber Fx)

Serviço de carteiras para operações financeiras de provedores de jogos, com
**garantias distribuídas**: idempotência persistente, ledger append-only,
transactional outbox, inbox durável e controle de concorrência por carteira.

## Stack

| Responsabilidade | Escolha |
| --- | --- |
| Linguagem | **Go 1.26** (`go.mod` e `Dockerfile` declaram a versão) |
| Composição | **Uber Fx** (`fx.Module`, `fx.Provide`, `fx.Invoke`, `fx.Lifecycle`) |
| Transport | **go-kit** (`kit/endpoint` + `kit/transport/http` + `kit/metrics`) sobre `net/http` |
| Persistência | **PostgreSQL 17** com `pgx/v5` e SQL explícito |
| Mensageria | **AWS SQS (FIFO)** via LocalStack |
| IdP | **Keycloak** (OAuth2/OIDC, `client_credentials`) |
| Migrations | **goose** embarcadas no binário |
| Métricas | **go-kit metrics** (`kit/metrics` + provider Prometheus) em `/metrics`, logs JSON estruturados (`slog`) |
| Testes unitários | **testify** (`require`/`assert`) com testes table-driven |

> **Por que go-kit E Uber Fx?** O desafio exige Uber Fx para composição/ciclo
> de vida. O go-kit complementa sem conflito: endpoints (`kit/endpoint`)
> isolam casos de uso do transporte, `kit/transport/http` cuida de
> decode/encode/erros, os endpoint middlewares (`authorize`, `instrument`)
> são Decorators e `kit/metrics` instrumenta com Prometheus. O domínio
> (`internal/domain`) não conhece nenhuma das duas.

## Estrutura do projeto

Layout hexagonal (ports & adapters) com os nomes de pacote idiomáticos da
comunidade Go e o vocabulário do go-kit:

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
  platform/             # config, logging (slog), metrics (kit/metrics + Prometheus)
  bootstrap/            # composition root: fx.Modules + adapter de instrumentação
  migrations/           # SQL goose embarcado (go:embed)
test/integration/       # suíte end-to-end contra Postgres/Keycloak/LocalStack reais
```

Padrões de design aplicados: **Ports & Adapters** (hexagonal), **Repository**,
**Unit of Work** (`TxManager`), **Decorator** (endpoint middlewares, http
middlewares, access log), **Strategy** (dispatch por `TxKind`), **State**
(máquina de estados da transação), **Observer** (port de instrumentação),
**Adapter** (instrumentação → kit/metrics, SQS/pgx/OIDC), **Factory**
(envelopes de eventos, rotas, métricas) e **Functional Options**
(`WithReferencePolicy`, `WithLedgerPageSize`).

## Subindo o ambiente

Pré-requisitos: Docker (ou Podman) + Compose, Go 1.26+.

```sh
docker compose up --build          # postgres + keycloak + localstack + app
```

O serviço fica em **http://localhost:8081** (Keycloak em 8080, LocalStack em
4566, Postgres em 5433). O container da aplicação aplica as migrations no
start, provisiona o realm `wallet` no Keycloak (import automático) e cria as
filas SQS via script de init do LocalStack.

### Instâncias múltiplas (teste de concorrência distribuída)

```sh
docker compose --profile multi up --build -d   # sobe app-1 (:8081), app-2 (:8082), app-3 (:8083)
```

### Variáveis de ambiente

Todas as variáveis e valores locais estão em [`.env.example`](.env.example).
As principais: `WALLET_DB_DSN`, `WALLET_SQS_ENDPOINT`, `WALLET_AUTH_ISSUER`,
`WALLET_AUTH_JWKS_URL` (override do JWKS para ambientes com hosts distintos),
`WALLET_WORKERS_ENABLED`, `WALLET_REFERENCE_TTL`/`WALLET_REFERENCE_MAX_ATTEMPTS`.

## Identidades de teste (Keycloak)

O realm `wallet` é importado automaticamente (veja
`deploy/keycloak/realm-wallet.json`). Três clientes `client_credentials`:

| Cliente | Secret | Poder |
| --- | --- | --- |
| `provider-a` | `provider-a-secret` | Operações **apenas** com `providerId=provider-a` |
| `provider-b` | `provider-b-secret` | Operações **apenas** com `providerId=provider-b` |
| `wallet-internal` | `internal-secret` | Serviço interno: abre/lê carteiras, vê tudo |

Obtendo um token:

```sh
curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret \
  | jq -r .access_token
```

## Migrations

Aplicadas automaticamente no start (`WALLET_MIGRATE_ON_START=true`). Manual:

```sh
go run ./cmd/walletd migrate up      # aplica pendentes
go run ./cmd/walletd migrate down    # reverte UMA migration (reversível, passo a passo)
go run ./cmd/walletd migrate status  # lista estado
```

## Exemplos de chamada

```sh
TOKEN_INT=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=internal-secret | jq -r .access_token)
TOKEN_A=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret | jq -r .access_token)
API=http://localhost:8081

# 1. abrir carteira (interna)
curl -s -X POST $API/wallets -H "Authorization: Bearer $TOKEN_INT" -H 'Content-Type: application/json' \
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

Publicando na fila (contrato de entrada):

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
    "money": {"amount": "25.00", "currency": "BRL"}
  }
}
```

`MessageGroupId` = `walletId` (ordenação por carteira) e
`MessageDeduplicationId` = hash do corpo. Eventos de saída são publicados em
`integration-events.fifo` com `MessageGroupId` = `aggregateId` e
`MessageDeduplicationId` = `eventId`.

## Códigos HTTP

| Situação | Status | Corpo |
| --- | --- | --- |
| Entrada inválida (shape) | 400 | `{"error":{"code":"VALIDATION_FAILED",...}}` |
| Sem token / token inválido/expirado | 401 | `UNAUTHORIZED` |
| Autenticado sem permissão (ex.: provider lendo carteira) | 403 | `FORBIDDEN` |
| Carteira/transação inexistente | 404 | `WALLET_NOT_FOUND` / `TRANSACTION_NOT_FOUND` |
| Chave reutilizada com outro conteúdo; mesmo external id com outra chave; carteira duplicada | 409 | `IDEMPOTENCY_KEY_CONFLICT` / `EXTERNAL_TRANSACTION_ID_CONFLICT` / `WALLET_ALREADY_EXISTS` |
| Rejeição de negócio persistida (REJECTED) | 422 | `failureCode` + `failureMessage` (ex.: `INSUFFICIENT_FUNDS`, `DUPLICATE_REVERSAL`) |
| Pendência de referência | 202 | `status: PENDING_REFERENCE` + `nextRetryAt` |
| Sucesso / replay idempotente | 200 | `status`, `balance`, `idempotentReplay` |
| PostgreSQL/SQS indisponível | 503 | `TRANSIENT_UNAVAILABLE` |

Códigos de falha de negócio (estáveis, persistidos em `failure_code`):
`INSUFFICIENT_FUNDS`, `REVERSAL_INSUFFICIENT_FUNDS` (diferente do anterior,
por exigência do desafio), `CURRENCY_MISMATCH`, `PLAYER_WALLET_MISMATCH`,
`REFERENCE_MISMATCH`, `REVERSAL_KIND_MISMATCH`, `REVERSAL_AMOUNT_MISMATCH`,
`DUPLICATE_REVERSAL`, `REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED`.

## Testes

Unitários com **testify**, table-driven, cobrindo os cinco pacotes de núcleo
(domínio, casos de uso, transporte, mensageria e auth — os fakes dos ports
demonstram a substituibilidade da arquitetura hexagonal):

```sh
go test ./...            # unitários (testify)
go test -race ./...      # com detector de corrida
go vet ./...
```

### Testes de integração (containers reais — sem mock de infra)

```sh
docker compose up -d postgres keycloak localstack   # dependências
make test-integration                                # go test -tags=integration ./test/integration/...
```

Cobrem: migrations e constraints (ledger append-only, unicidades, CHECKs),
atomicidade financeira, cenário obrigatório de concorrência (100.00 e duas
apostas de 80.00, inclusive com **duas instâncias**), replays HTTP+SQS
cruzados, inbox/outbox, DLQ, resolução de referências pendentes, expiração de
referência (TTL), isolamento entre provedores e rejeição de credenciais, e
composição/ciclo de vida Fx.

### Simulações de falha

| Falha | Como simular | Garantia observável |
| --- | --- | --- |
| Consumidor morre pós-commit, pré-delete | `docker compose kill app` durante carga; mensagens voltam pela visibility timeout | inbox absorve a reentrega; saldo não duplica |
| Publicador morre entre commit e publicação | `docker compose pause app` após criar operação e antes do worker publicar; depois `unpause` | outbox publica com o mesmo `eventId` |
| Dois publishers disputando a outbox | duas instâncias (`--profile multi`) com fila de eventos vazia | `FOR UPDATE SKIP LOCKED` + dedup por `eventId` |
| Referência antes da transação | enviar `REFUND` antes do `BET` | `PENDING_REFERENCE` → resolução automática (ou `REFERENCE_NOT_FOUND` após TTL) |
| Reinício da aplicação | `docker compose restart app` | idempotência e pendências sobrevivem (persistidas no Postgres) |

## Decisões técnicas

Detalhadas em [`ARCHITECTURE.md`](ARCHITECTURE.md) — dinheiro, transações,
idempotência, locks, referências pendentes, reversões, inbox/outbox,
autenticação, Fx e shutdown.

## Formatação

```sh
gofmt -l .   # vazio = formatado
```
