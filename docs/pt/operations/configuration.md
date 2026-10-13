> Toda a configuração do serviço entra por **variáveis de ambiente**,
> lidas uma única vez na inicialização e validadas antes de o processo subir. O
> arquivo `internal/platform/config/config.go` é a fonte da verdade:
> esta página documenta cada variável, seu *default* real e o efeito de
> mudá-la.

## A regra: default funcional, falha cedo

`config.Load()` lê o ambiente, aplica os *defaults* e roda `validate()`. Se algo
estiver inválido, o Fx **recusa o start** — não há boot parcial com configuração
meio aplicada.

> [!NOTE]
> **Por que validar na carga, não no uso**
>
> Um DSN vazio descoberto só na primeira query vira um erro intermitente sob
> carga. Validar na partida transforma o erro em **falha de boot**, que é
> imediata, reprodutível e visível no `docker compose logs`.


```sh
# O binário lê o ambiente; nada é lido de arquivo por padrão.
WALLET_MODE=api WALLET_LOG_LEVEL=debug ./walletd
```

> [!TIP]
> **Defaults que funcionam localmente**
>
> Os *defaults* apontam para a stack de `docker-compose.yml` (Postgres em
> `5433`, LocalStack em `4566`, Keycloak em `8080`, API em `8080`). Rodar
> `make up` e depois o binário **sem nenhuma variável** já é um ambiente
> completo.


## Aplicação e ciclo de vida

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_MODE` | `all` | Seleciona `all`, `api` ou `worker`. Ver a tabela de modos abaixo. |
| `WALLET_HTTP_ADDR` | `:8080` | Endereço de escuta HTTP. Obrigatório: vazio é erro de boot. |
| `WALLET_SHUTDOWN_GRACE` | `20s` | Janela para encerrar workers e in-flight antes do `SIGKILL`. |
| `WALLET_LOG_LEVEL` | `info` | Um de `debug`, `info`, `warn`, `error`. Qualquer outro valor é erro de boot. |

| `WALLET_MODE` | O que sobe |
| :-- | :-- |
| `all` | HTTP + consumer + outbox relay + reference worker + reconcile |
| `api` | Apenas o servidor HTTP (probes e `/metrics` continuam de pé) |
| `worker` | Apenas os workers de background |

> [!WARNING]
> **`WALLET_WORKERS_ENABLED` é independente de `WALLET_MODE`**
>
> O modo decide **o que o processo pode** rodar; `WALLET_WORKERS_ENABLED=false`
> desliga os workers mesmo em `all`. Use o primeiro para separar processos,
> o segundo para silenciar os workers num teste pontual.


## Banco de dados

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_DB_DSN` | `postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable` | Conexão pgx. Obrigatório: vazio é erro de boot. |
| `WALLET_DB_MAX_CONNS` | `20` | Teto do pool de conexões. Deve ser `>= 1`. |
| `WALLET_MIGRATE_ON_START` | `true` | Aplica as migrations de `internal/migrations` na subida. |

> [!NOTE]
> **`WALLET_DB_MAX_CONNS` versus `WALLET_CONSUMER_CONCURRENCY`**
>
> Cada mensagem consumida abre transação e segura uma conexão enquanto roda.
> Com 4 consumidores, um pool de 20 sobra; com 32 consumidores, um pool de 20
> faz a fila do pool virar o gargalo — e o sintoma é latência, não erro.


## SQS

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_SQS_ENDPOINT` | `http://localhost:4566` | Endpoint do SQS (LocalStack local, AWS na nuvem). |
| `WALLET_SQS_REGION` | `us-east-1` | Região do cliente. Obrigatória. |
| `WALLET_SQS_ACCESS_KEY_ID` | `test` | Credencial estática (LocalStack aceita qualquer valor). |
| `WALLET_SQS_SECRET_KEY` | `test` | Par da credencial acima. |
| `WALLET_SQS_REQUEST_QUEUE` | `wager-transactions.fifo` | Fila de entrada de apostas. Precisa terminar em `.fifo`. |
| `WALLET_SQS_EVENTS_QUEUE` | `integration-events.fifo` | Fila de saída do outbox. Precisa terminar em `.fifo`. |
| `WALLET_SQS_DLQ_QUEUE` | `wager-transactions-dlq.fifo` | Dead-letter queue de `wager-transactions.fifo`. |
| `WALLET_SQS_WAIT_SECONDS` | `5` | Long polling, dentro de `[0,20]`. |
| `WALLET_SQS_MAX_MESSAGES` | `10` | Tamanho do lote por `ReceiveMessage`, dentro de `[1,10]`. |
| `WALLET_SQS_VISIBILITY` | `60` | Visibility timeout em segundos durante o processamento. |
| `WALLET_SQS_MAX_RECEIVE` | `5` | Recebimentos antes de a mensagem ir para a DLQ (redrive). |

> [!WARNING]
> **As três filas precisam existir**
>
> A validação exige `REQUEST_QUEUE` e `EVENTS_QUEUE` com sufixo `.fifo`; um
> nome sem `.fifo` é **erro de boot**, não warning. As filas em si são criadas
> por `deploy/localstack/init/10-queues.py` (via `make up`) ou por
> `deploy/tofu` — não pelo binário.


## Workers

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_WORKERS_ENABLED` | `true` | Liga/desliga todos os workers de background. |
| `WALLET_CONSUMER_CONCURRENCY` | `4` | Goroutines consumindo a fila de requests em paralelo. |
| `WALLET_OUTBOX_INTERVAL` | `500ms` | Período do tick do relay do outbox. |
| `WALLET_OUTBOX_BATCH` | `100` | Máximo de eventos reclamados por tick. |
| `WALLET_OUTBOX_LEASE` | `30s` | Posse do claim antes de outro publisher poder retomá-lo. |
| `WALLET_REFERENCE_INTERVAL` | `1s` | Período do tick do reference worker. |
| `WALLET_REFERENCE_BATCH` | `50` | Máximo de referências resolvidas por tick. |
| `WALLET_REFERENCE_MAX_ATTEMPTS` | `10` | Tentativas antes de a referência virar falha permanente. |
| `WALLET_REFERENCE_TTL` | `15m` | Teto de idade de uma referência pendente antes de ser expirada. |
| `WALLET_RECONCILE_INTERVAL` | `5m` | Período da varredura de reconciliação. |
| `WALLET_RECONCILE_BATCH` | `100` | Carteiras por lote na varredura (memória e duração do read). |

> [!NOTE]
> **Intervalo pequeno, não zero**
>
> O relay do outbox é *polling* com `500ms` de período. O evento é gravado na
> **mesma transação** da aposta e só então publicado — o intervalo não afeta
> correção, apenas a latência até o evento sair. Baixá-lo reduz latência e
> aumenta o número de queries vazias.


## Autenticação

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_AUTH_ISSUER` | `http://localhost:8080/realms/wallet` | Issuer OIDC esperado no `iss`. Obrigatório. |
| `WALLET_AUTH_JWKS_URL` | *(vazio)* | Quando vazio, derivado do issuer. Preencher só quando o JWKS não for padrão. |
| `WALLET_AUTH_AUDIENCE` | *(vazio)* | Checagem de `aud`. Vazio **desliga** a checagem; o compose define `wallet-api`. |

> [!WARNING]
> **`WALLET_AUTH_ISSUER` é obrigatório**
>
> OAuth2/OIDC não é opcional: sem issuer o serviço não sobe. É uma decisão de
> desenho — autenticação é requisito, não *feature flag*.


## Paginação

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_LEDGER_PAGE_MAX` | `200` | Teto de itens por página no extrato. Limita o custo de uma listagem. |

## OpenTelemetry

Telemetria é **opt-in**: com `WALLET_OTEL_ENABLED=false` o serviço roda exatamente
como antes e nenhum ambiente depende de um collector de pé.

| Variável | Default | Efeito |
| :-- | :-- | :-- |
| `WALLET_OTEL_ENABLED` | `false` | Liga traces/logs OTLP. Desligado, o exporter é um no-op. |
| `WALLET_OTEL_ENDPOINT` | `localhost:4317` | Host:porta do collector (OTLP/gRPC). |
| `WALLET_OTEL_INSECURE` | `true` | Sem TLS na exportação (correto para o collector local). |
| `WALLET_OTEL_SERVICE_NAME` | `wallet` | Valor de `service.name` nos recursos. |
| `WALLET_ENV` | `local` | Valor de `deployment.environment`. |
| `WALLET_INSTANCE_ID` | *hostname* | Distingue réplicas; cair para hostname mantém o valor estável. |
| `WALLET_OTEL_SAMPLE_RATIO` | `1.0` | Fração amostrada. Reduzir em produção de alto volume. |
| `WALLET_OTEL_LOGS_ENABLED` | `true` | Exporta logs para o Loki via collector. |
| `WALLET_OTEL_EXPORT_TIMEOUT` | `5s` | Timeout por exportação. |
| `WALLET_OTEL_BATCH_TIMEOUT` | `2s` | Intervalo do BatchSpanProcessor. |
| `WALLET_OTEL_MAX_QUEUE_SIZE` | `4096` | Teto da fila do processor. |

> [!TIP]
> **Desligar telemetria não muda o comportamento**
>
> Os spans são registrados na porta `Instrumentation`, cujo no-op não tem
> custo de rede. Isso permite rodar a suíte rápida **sem** a stack de
> observabilidade no ar.


## Um ambiente completo, do zero

```sh
# Sobe Postgres, LocalStack (filas) e Keycloak.
make up

# O binário roda com os defaults; nada a exportar.
go run ./cmd/walletd

# Modo worker isolado, apontando para um Postgres remoto:
WALLET_MODE=worker \
WALLET_DB_DSN="postgres://wallet:wallet@db.internal:5432/wallet?sslmode=require" \
WALLET_SQS_ENDPOINT="https://sqs.us-east-1.amazonaws.com" \
WALLET_OTEL_ENABLED=true \
WALLET_OTEL_ENDPOINT="otel.internal:4317" \
./walletd
```

Veja também: [Observabilidade](observability.md) ·
[Infraestrutura como código](infrastructure.md) ·
[Solução de problemas](troubleshooting.md).
