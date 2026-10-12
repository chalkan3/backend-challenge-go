# Architecture — Wallet Service

Decisões de arquitetura do desafio, organizadas pelos tópicos exigidos.

## 1. Composição (Uber Fx) e go-kit

- `internal/bootstrap` monta a aplicação com `fx.Module` por camada:
  `platform` (config, log, metrics), `auth`, `postgres`, `sqs`, `usecases`,
  `http`, `workers`. Toda dependência entra por injeção de construtor —
  nenhum `init()` ou global mutável.
- **Ciclo de vida** (`fx.Lifecycle`): a ordem de *start* é migrations →
  servidor HTTP → workers; o *stop* é o inverso (hooks rodam em ordem
  reversa de registro): workers drenam → HTTP para de aceitar → **pool do
  Postgres fecha por último**. Migrations rodam no `OnStart` (goose, SQL
  versionado com down).
- **go-kit** entra no transporte e observabilidade: cada caso de uso é um
  `endpoint.Endpoint`; middlewares de endpoint aplicam autorização e
  instrumentação; `kit/transport/http` faz decode/encode e mapeia erros para
  HTTP; `kit/metrics` publica Prometheus. O `main` é
  `fx.New(bootstrap.App).Run()` — não há lógica no main.
- O domínio (`internal/domain`) não importa Fx, HTTP, SQS nem pgx. A camada
  de aplicação (`internal/app`) só conhece as portas (`ports.go`) e o
  domínio.

## 2. Dinheiro (`Money`)

- `int64` em **unidades mínimas (centavos)** + código ISO 4217. Nenhum
  `float32/float64` em parsing, cálculo, serialização ou persistência: o
  parsing é textual (`ParseMoney` com regex), a coluna é `BIGINT`
  (`*_minor`), e o JSON usa **string decimal** com escala fixa 2.
- Overflow é verificado explicitamente em Add/Sub/Neg
  (`ErrMoneyOverflow`); o parsing rejeita >16 dígitos inteiros e >2 casas
  decimais (nunca arredonda). Limite teórico: ±92 quintilhões de centavos
  (faixa do `int64`), 16 dígitos na forma textual.
- Moedas diferentes nunca se combinam (`ErrCurrencyMismatch`). Escala fixa 2
  para todas as moedas: moedas com 0 ou 3 casas (JPY, KWD) não são
  suportadas — limitação documentada; os cenários do desafio operam em BRL.

## 3. Transações SQL e concorrência

- **Um único limite de transação**: `TxManager.WithinTx` (pgx). Estado da
  transação, saldo, ledger, inbox e outbox commitam juntos — ou nada
  commitou.
- Concorrência por **lock pessimista por carteira**: `SELECT ... FOR UPDATE`
  na linha da carteira dentro do caso de uso. Carteiras independentes nunca
  disputam o mesmo lock (paralelismo preservado; **nenhum lock global**).
  A defesa final é do banco: `CHECK (balance_minor >= 0)` na carteira e no
  ledger — mesmo com um bug de aplicação, saldo negativo é impossível.
- Lost updates são impedidos pela serialização do `FOR UPDATE` (a versão da
  carteira é incrementada apenas quando o saldo muda e serve de auditoria;
  o UPDATE do saldo sempre acontece sob o lock da própria linha).
- A reconciliação (`POST /wallets/:id/reconciliation`) compara saldo
  armazenado × soma do ledger **em um único statement SQL** (snapshot
  consistente, sem lock) e nunca altera estado; divergência vira log ERROR,
  resposta e métrica `wallet_reconciliation_divergences_total`.

## 4. Idempotência

- **Persistente e no banco**, sobrevivendo a restart de tudo:
  - Chave: `UNIQUE (provider_id, idempotency_key)`;
  - Identidade financeira: `UNIQUE (provider_id, external_transaction_id)`
    (uma operação não pode ser reaplicada com outra chave);
  - **Hash determinístico do payload** (SHA-256 sobre JSON canônico com
    chaves ordenadas; exclui a chave de idempotência e metadados de
    transporte; dinheiro normalizado — `"25"`, `"25.0"` e `"25.00"` são o
    mesmo hash). Mesma chave + mesmo hash → **replay** devolve o resultado
    original persistido com `idempotentReplay: true` (inclusive o saldo
    observado no processamento original, guardado em
    `balance_before/after_minor` e `wallet_version_after`); mesmo hash não →
    **409**.
- HTTP e SQS compartilham o MESMO caso de uso (`WagerService`), logo a
  deduplicação cruzada entre canais é automática. A inbox (abaixo) é uma
  segunda camada, específica da entrega at-least-once do SQS.
- Ao perder uma corrida de insert (violção de unicidade sob concorrência),
  o perdedor relê a linha vencedora e responde como replay — nunca erro.

## 5. Máquina de estados e referências pendentes

- `PENDING → PENDING_REFERENCE | PROCESSED | REJECTED | FAILED`;
  `PENDING_REFERENCE → (retry) | PROCESSED | REJECTED | FAILED`. Estados
  terminais nunca mudam (validação no domínio + guard `WHERE status IN
  ('PENDING','PENDING_REFERENCE')` no UPDATE persistente).
- Operações síncronas vão direto ao estado final **no commit único** (sem
  aceite intermediário). Quando a referência (`REFUND`/`ROLLBACK`) ainda não
  chegou, a operação persiste como `PENDING_REFERENCE` com
  `next_retry_at` — retomada durável por qualquer instância.
- O **ReferenceWorker** varre `PENDING_REFERENCE` com
  `FOR UPDATE SKIP LOCKED`, backoff exponencial (1s→30s), máximo de
  tentativas (`WALLET_REFERENCE_MAX_ATTEMPTS`) e TTL
  (`WALLET_REFERENCE_TTL`): expirou → `REJECTED` com
  `REFERENCE_NOT_FOUND` + evento `WagerTransactionRejected`.
- Referência existe mas está `PENDING`/`PENDING_REFERENCE` → continua
  esperando; terminou sem sucesso (`REJECTED`/`FAILED`) → rejeição imediata
  com `REFERENCE_NOT_PROCESSED`.

## 6. Reversões

- `REFUND` devolve integralmente uma `BET` processada; `ROLLBACK` inverte
  `BET` (crédito), `WIN` (débito) ou `REFUND` (débito). Exige concordância
  de provedor, jogador, carteira, moeda e rodada com a referência; valor
  igual ao referenciado (sem reversão parcial).
- **Uma reversão bem-sucedida por tipo por referência**, imposta por índice
  único parcial (`kind='REFUND'|'ROLLBACK' AND status='PROCESSED'`), além da
  checagem no caso de uso (`DUPLICATE_REVERSAL`).
- Reversão que precisaria debitar mais que o saldo é rejeitada com
  `REVERSAL_INSUFFICIENT_FUNDS` — código distinto de `INSUFFICIENT_FUNDS`
  (exigência do desafio) — e fica auditável como transação `REJECTED`.
- `LOSS` não move dinheiro: exige `amount = 0.00`, não cria ledger nem muda
  versão, e produz `WagerTransactionProcessed` sem `WalletBalanceChanged`.

## 7. Inbox (entrada SQS)

- `consumer_inbox (consumer_name, message_id PK, hash, received_at,
  completed_at)` — o insert da inbox **compartilha a transação SQL** dos
  efeitos de domínio. Linha commitada ⇒ mensagem tratada integralmente.
- Redelivery (crash pós-commit, pré-delete): a inbox já existe com hash
  igual e `completed_at` setado ⇒ ack sem reprocessar. Hash diferente para o
  mesmo `messageId` é reuso indevido de identidade ⇒ DLQ.
- A mensagem é **removida da fila somente após o commit**. Falhas
  transitórias não fazem delete (visibility timeout reentrega); após
  `maxReceiveCount` (redrive = 5) a mensagem vai à DLQ. Mensagens inválidas
  (envelope/shape) vão direto à DLQ com métrica.
- `SIGTERM`: o consumidor para de buscar mensagens e espera o trabalho em
  voo terminar dentro do prazo (`WALLET_SHUTDOWN_GRACE`); se estourar, solte
  a mensagem sem delete — a reentrega é segura por causa da inbox.

## 8. Outbox (saída)

- Eventos (`WagerTransactionProcessed`, `WagerTransactionRejected`,
  `WalletBalanceChanged`, `WagerTransactionPendingReference`) são gravados
  em `integration_outbox` **na mesma transação** do domínio — publicação
  só acontece depois do commit (o worker só lê linhas commitadas).
- O **OutboxWorker** disputa lotes com `FOR UPDATE SKIP LOCKED` + lease
  (`available_at = now() + lease`, `attempts++`): publishers múltiplos não
  duplicam trabalho; um publisher que morre após o claim tem a linha
  recuperada por outra instância quando o lease expira.
- Retry com backoff exponencial (cap 1h) e `last_error` auditável.
  Republicação preserva o `eventId` (PK); no SQS,
  `MessageDeduplicationId = eventId` absorve republicações. Destino:
  `integration-events.fifo` com `MessageGroupId = aggregateId` (ordem por
  carteira, paralelismo entre carteiras).
- O envelope tem `eventId`, `eventType`, `aggregateType`, `aggregateId`,
  `correlationId`, `occurredAt` (UTC RFC 3339), `version` (schema do evento)
  e `data` tipado — snapshot imutável do momento do commit.

## 9. Autenticação e autorização

- **Keycloak** como IdP (recomendação do desafio), fluxo
  `client_credentials` service-to-service. O serviço nunca emite token nem
  cadastra senha — valida via **JWKS** (`lestrrat-go/jwx`) com cache e
  refresh automático; assinatura, `iss` e `exp` são obrigatórios (`aud`
  opcional).
- Modelo de permissões: a identidade **determina o `providerId`** — o claim
  `providerId` é injetado por mapper *hardcoded* em cada cliente provider
  (impossível forjar via request). `wallet-internal` carrega o claim
  `wallet:internal`. Regras:
  - Rotas de carteira (`/wallets*`) → **somente interno** (403 para
    providers);
  - Submissões e leituras de transação → o `providerId` do corpo/rota tem
    que bater com o do token (403), inclusive em **replays e consultas**;
  - Isolamento também vale para referências: provider-b não consegue
    resolver referência de provider-a (fica pendente e nunca credita).
- Mensageria: credenciais de teste do LocalStack no compose; em produção o
  acesso à fila seria controlado por IAM/policies do broker. As validações
  de domínio permanecem no consumidor (o token viaja no payload do evento
  de negócio, e a identidade do publicador é responsabilidade do broker).

## 10. Observabilidade

- Logs JSON (`slog`) com `correlationId`, `messageId`, `transactionId`,
  `walletId` e `providerId` quando disponíveis; sem payloads financeiros
  completos nem credenciais.
- Métricas Prometheus (`/metrics`): resultado por status/kind/canal,
  duplicatas por origem, retries, DLQ, conflitos, atraso e pendência da
  outbox, latência por endpoint (go-kit histogram) e divergências de
  reconciliação. Health: `/health/live` (processo) e `/health/ready`
  (Postgres + SQS).

## 11. Limitações e trabalho não concluído

- Escala monetária fixa 2 (sem JPY/KWD); sem conversão de moeda.
- Tracing OpenTelemetry e dashboards não implementados (diferenciais
  opcionais); partidas dobradas no ledger também não (opcional).
- Emulação de falha "kill -9" está documentada via `docker compose kill`,
  mas não há um harness automatizado dedicado para o cenário 5/6 do §13
  (os demais cenários têm teste).
- O mapeamento `providerId ↔ cliente Keycloak` usa mappers hardcoded no
  realm de teste; em produção, grupos/roles do IdP.
- Credenciais LocalStack (test/test) e secrets de realm são de ambiente
  local, versionados de propósito para reprodutibilidade.
