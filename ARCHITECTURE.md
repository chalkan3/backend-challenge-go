# Architecture — Wallet Service

Decisões de arquitetura do desafio, organizadas pelos tópicos exigidos.

## 0. Layout e padrões de design

Layout hexagonal (ports & adapters), com pacotes nomeados no estilo da
comunidade Go e vocabulário do go-kit:

| Pacote | Responsabilidade |
| --- | --- |
| `internal/domain` | Modelo puro: `Money` (value object), `Wallet` (aggregate root), `WagerTransaction` (máquina de estados), `LedgerEntry`, envelopes de eventos, erros com código estável, **Specifications** (`specification.go`, `spec_transaction.go`) |
| `internal/service` | Casos de uso + **ports** (`ports.go`: repositórios, `TxManager`, `Instrumentation`), erros classificados e **Strategy registry** (`strategy.go`) |
| `internal/transport/http` | go-kit: `Endpoints`, decoders/encoders, error encoder, middlewares de endpoint e de rede |
| `internal/messaging` | Workers de fundo: consumer SQS (inbox), outbox relay, reference resolver |
| `internal/postgres` | Adapter pgx: pool, migrations goose, repositórios, store (unit of work) |
| `internal/sqs` | Adapter SQS: client e publisher da outbox |
| `internal/auth` | Adapter OIDC (jwx + JWKS): `Principal`, validação de tokens |
| `internal/platform/{config,logging,metrics}` | Cross-cutting: configuração validada, slog JSON, métricas **kit/metrics** sobre Prometheus |
| `internal/bootstrap` | Composition root: `fx.Module` por camada + adapter de instrumentação (ports → métricas) |
| `test/integration` | Suíte `-tags=integration` contra Postgres/Keycloak/LocalStack reais |

Padrões de design explícitos:

- **Ports & Adapters (hexagonal)** — domínio no centro; driving adapter HTTP,
  driven adapters postgres/sqs/auth; ports definidos no lado do serviço.
- **Repository** — `WalletRepository`, `TransactionRepository`,
  `LedgerRepository`, `InboxRepository`, `OutboxRepository`.
- **Unit of Work** — `TxManager.WithinTx`: toda escrita financeira (linhas de
  domínio + ledger + inbox + outbox) em uma única transação SQL.
- **Decorator** — endpoint middlewares (`authorize`, `instrument`), middlewares
  de rede (`auth`, `logRequests`), workers empilhados por composição.
- **Strategy + Registry** — dispatch por `TxKind` via `service.StrategyRegistry`
  (`strategy.go`): um `wagerStrategy` por tipo é registrado no registry, não
  num `map` inline. O pipeline compartilhado (idempotência, lock da carteira,
  persistência) fica livre de condicionais por tipo, e adicionar um tipo novo
  é *Open/Closed*: registra-se a estratégia sem editar o pipeline. O registry
  é substituível via `WithStrategyRegistry` (Functional Options), o ponto de
  extensão do caso de uso.
- **Specification** — regras de negócio como predicados nomeados e componíveis
  (`domain/specification.go`, `domain/spec_transaction.go`). A "forma" de uma
  operação externa é um `And(...)` de regras (kind, identidade, política de
  valor zero/positivo, referência obrigatória), avaliadas numa ordem fixa que
  preserva o código de erro publicado. Cada regra é testável isoladamente e a
  composição é verificada por teste (`specification_test.go`).
- **State** — máquina de estados documentada (`allowedTransitions`) com
  transições validadas no agregado.
- **Observer** — port `Instrumentation`/`ConsumerMetrics`: casos de uso e
  workers publicam resultados sem conhecer métricas.
- **Adapter** — `bootstrap/instrumentation.go` (ports → kit/metrics);
  pgx/SQS/OIDC adaptados aos ports.
- **Factory Method** — construtores de eventos de integração, rotas HTTP
  declarativas, helpers de métricas.
- **Functional Options** — `WithReferencePolicy`, `WithLedgerPageSize`,
  `WithStrategyRegistry`; configuração entra no caso de uso como valor
  simples, injetada pelo composition root.
- **Value Object / Aggregate (DDD)** — `Money` imutável; `Wallet` só muda via
  `Credit`/`Debit`, que preservam os invariantes e produzem `Movement`.

Testes unitários usam **testify** com fakes dos ports (substituibilidade da
arquitetura), table-driven e sem I/O.

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
  de aplicação (`internal/service`) só conhece as portas (`ports.go`) e o
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
- Uma varredura **contínua** (worker de reconciliação) passa todas as carteiras
  em lotes keyset a cada `WALLET_RECONCILE_INTERVAL`, usando o **mesmo** caso de
  uso `Reconcile` da rota HTTP — a reconciliação sob demanda só acha o que
  alguém pergunta, a varredura transforma a detecção em propriedade do sistema
  rodando. É somente-leitura: relata (`reconciliation_divergences_total`), nunca
  corrige — o ledger é a única fonte de verdade, uma divergência é fato a
  expor, não número a maquiar.
- **Partidas dobradas**: além da perna unilateral, cada movimento grava sua
  contraparte na tabela `postings`, na mesma transação do saldo
  (`00004_postings.sql`; escrito em `finishProcessed` e na abertura de
  carteira). O journal é imutável (triggers recusam `UPDATE`/`DELETE`) e a perna
  do jogador é **amarrada à linha do ledger** — um posting de `PLAYER_WALLET`
  sem a linha correspondente (mesma carteira, direção e valor) é rejeitado pelo
  banco. O que a perna unilateral não consegue expressar, e que aqui passa a ser
  auditável, é a conservação de valor: auditorias #11 (a entrada fecha), #11b
  (todo movimento tem posting) e #11c (as pernas concordam com o histórico).

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
- **Abertura de carteira (`POST /wallets`) segue o mesmo modelo.** Quando o
  cabeçalho `Idempotency-Key` está presente, a chave é gravada em
  `wallet_open_requests (idempotency_key PK, payload_hash, wallet_id)` **na
  mesma transação** que a carteira, a transação `OPENING`, o lançamento no
  ledger e os eventos de outbox. Repetir a requisição (mesmo hash do payload,
  calculado com a mesma normalização de dinheiro) devolve a carteira original
  com `idempotentReplay: true`, **sem** segunda carteira nem crédito duplicado;
  reutilizar a chave com outro corpo devolve `409 IDEMPOTENCY_KEY_CONFLICT`.
  Sem chave, a unicidade `(player_id, currency)` continua sendo a guarda
  (`409 WALLET_ALREADY_EXISTS`) — a chave adiciona replay do **sucesso**, que
  a unicidade sozinha não consegue oferecer.
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
  cadastra senha — valida via **JWKS** (`lestrrat-go/jwx`); assinatura, `iss` e
  `exp` são obrigatórios (`aud` opcional).

### Resiliência do JWKS: por que `jwk.Fetch` e não `jwk.Cache`

A primeira versão usava `jwk.Cache` com um *warm-up* em goroutine. Um probe
lado a lado — mesmo servidor JWKS, mesma janela de 3 s, contando acessos à rede
— mostrou que os dois não se comportam igual quando o IdP está fora no boot:

```
jwk.Cache em loop :  1 acesso à rede em 3s   (a 1ª falha é cacheada)
jwk.Fetch em loop :  2 acessos à rede em 3s  (retenta de verdade)
```

`jwk.Cache.Get` **cacheia o erro** da primeira busca e passa a responder
`"cached object is not a Set"` sem tocar a rede durante toda a janela de
refresh. Contra um IdP que sobe depois do serviço — justamente a degradação que
este serviço deve sobreviver — o *warm-up* girava contra esse erro em cache e o
key set **nunca carregava**; todo token era recusado para sempre. A própria doc
do `jwx` recomenda não usar `jwk.Cache` para JWKS que podem estar indisponíveis.

O carregador agora usa `jwk.Fetch` (que sempre vai à rede) num loop de retry com
backoff, e guarda o key set atrás de um **`atomic.Pointer[jwk.Set]`**:

- o refresher publica um set novo com um único `Store` atômico;
- cada `Validate` faz um único `Load` — sem lock compartilhado;
- a rotação é transparente (refresh periódico), e uma falha de refresh **mantém
  o último set bom** em vez de trancar todos para fora.

A troca também eliminou uma condição de corrida: o campo `set` era escrito pela
goroutine de fundo e lido sem sincronização pelas goroutines de request — só
disparava com o IdP fora no boot, invisível no caminho feliz. O
`atomic.Pointer` fecha isso e há teste sob `-race`.

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
- Policies do token (ver `deploy/keycloak/README.md`): a autorização é por
  **claim** (`providerId` / `wallet:internal`), que é *assinada* e logo
  infalsificável — o escopo **não** é portão, apenas higiene. O realm emite
  escopo enxuto (`fullScopeAllowed: false`, client scopes `wallet:provider` /
  `wallet:internal`), de modo que `email`/`profile`/roles não entram num token
  de máquina, e injeta `aud=wallet-api`; o serviço **exige** essa audience
  (`WALLET_AUTH_AUDIENCE`), e um token emitido para outro público no mesmo
  realm é recusado. Política de token não é decorativa: é provada contra o
  realm vivo em `test/integration/keycloak_policy_test.go`.
- Mensageria: credenciais de teste do LocalStack no compose. As políticas de
  fila aplicadas (redrive para DLQ, long polling, retenção, dedup por id de
  evento) são provisionadas por `deploy/localstack/init/10-queues.py` e
  provadas — atributo lido de volta **e** comportamento (mensagem observada
  chegando à DLQ) — em `test/integration/sqs_policy_test.go`. **Resource
  policy e IAM não são aplicados pelo LocalStack community** (medido: uma
  `Deny *` foi gravada e o `SendMessage` passou); por isso não há nenhuma
  provisionada aqui, e há um teste que fixa esse limite. Em AWS real, o acesso
  à fila seria controlado por resource policy + IAM por serviço. As validações
  de domínio permanecem no consumidor (o token viaja no payload do evento
  de negócio, e a identidade do publicador é responsabilidade do broker).

## 10. Observabilidade

Três sinais, três donos — nenhum deles duplica o outro:

| Sinal | Onde vive | Quem consulta |
| --- | --- | --- |
| Métricas | go-kit metrics → Prometheus em `/metrics` | Prometheus → Grafana |
| Traces | OTLP/gRPC → Collector → Tempo | Grafana (TraceQL) |
| Logs | `slog` JSON em stdout (+ espelho OTLP → Loki) | `jq`/Loki, correlacionados por `trace_id` |

- Logs JSON (`slog`) com `correlationId`, `messageId`, `transactionId`,
  `walletId`, `providerId` e `trace_id`/`span_id` quando o tracing está
  ativo — é a chave de junção entre uma linha de log e seu trace.
- Métricas Prometheus (`/metrics`): resultado por status/kind/canal,
  duplicatas por origem, retries, DLQ, conflitos, atraso e pendência da
  outbox, latência por endpoint (go-kit histogram), falhas de auth por
  motivo e divergências de reconciliação. Health: `/health/live` (processo)
  e `/health/ready` (Postgres + SQS).
- **Tracing (OpenTelemetry)** é opt-in (`WALLET_OTEL_ENABLED`, default
  `false`): sem Collector o processo sobe normalmente com um tracer no-op,
  então nenhum call site precisa de nil check. Um trace HTTP atravessa
  transporte → caso de uso → repositório, com os spans do banco emitidos
  pelo `pgx` tracer injetado uma única vez no pool.
- **Continuidade do trace através da outbox.** A outbox quebra o trace em
  dois *por construção*: a requisição HTTP commita a linha e retorna,
  enquanto um relay destacado (`context.Background()`) publica depois.
  Nenhum contexto em memória liga as duas metades — o relay não pode
  reexecutar o que nunca viu. A ponte é **persistir o `traceparent` W3C**
  (coluna `integration_outbox.trace_parent`, escrita **na mesma transação**
  do saldo) e reabri-lo do outro lado:

  | Salto | O que carrega o vínculo |
  | --- | --- |
  | HTTP → outbox | `Enqueue` injeta o span ativo no `ctx` (o span do HTTP) |
  | outbox → relay | `ClaimBatch` devolve `trace_parent` **como dado** |
  | relay → SQS | `Publisher` o anexa como *message attribute* (metadado de transporte, não payload) |
  | SQS → consumer | `ReceiveMessage` pede o atributo e `Extract` o instala como parent remoto |

  O port `service.SpanCarrier` (implementado por `otel.Carrier` sobre
  `propagation.TraceContext{}` **explícito**, não o global) mantém o
  domínio e o `messaging` livres de imports do SDK — o formato é
  responsabilidade de adapter. Um `traceparent` vazio ou malformado
  **degrada para um trace novo**, nunca falha o trabalho: um produtor sem
  contexto a montante é um estado normal.

  > [!NOTE]
  > O atributo SQS só chega ao consumer se `ReceiveMessage` pedir
  > `MessageAttributeNames` explicitamente — atributos customizados não vêm
  > por padrão, ao contrário do corpo. Essa omissão era um bug real,
  > encontrado pelo teste e2e `TestConsumerResumesTraceFromMessageAttribute`.
- O **Collector** é o único ponto de ingestão: a aplicação nunca sabe qual
  backend armazena os traces. Trocar Tempo por Jaeger é mudar um YAML.
  Ele também **deriva** métricas dos spans (`spanmetrics`, `servicegraph`),
  o que dá latência/erro por span sem uma linha a mais de código e sem
  duplicar os contadores de domínio.
- Os **dashboards são código**: JSON versionado em
  `deploy/observability/grafana/dashboards`, provisionado no start com
  `allowUiUpdates: false`. O arquivo é a fonte da verdade, revisado como
  qualquer outro artefato.

Subindo a stack: `docker compose --profile observability up --build` (Grafana
em `:3000`, Prometheus em `:9090`, Tempo em `:3200`). Os traces só aparecem
com `WALLET_OTEL_ENABLED=true`.

## 11. Limitações e trabalho não concluído

- Escala monetária fixa 2 (sem JPY/KWD); sem conversão de moeda.
- O cenário 5 do §13 ("interromper o consumidor após o commit e antes do
  delete") tem harness automatizado — `test/integration/kill_after_commit_test.go`,
  determinístico via `commitGateTxManager` (sem depender de timing de
  `docker kill`). O cenário 6 ("dois publishers disputando a mesma outbox e
  recuperação de publicação") também tem teste dedicado —
  `test/integration/outbox_publishers_race_test.go`, exercitando a porta
  `ClaimBatch` concorrentemente (exclusão mútua do claim) e a recuperação após
  expiração do lease. A metade de recuperação é **provada por mutação**
  (remover o bump de `available_at` faz o teste falhar 3/3); a metade de
  exclusão mútua é verificada, mas **não** distingue `SKIP LOCKED` de um
  `FOR UPDATE` bloqueante — está documentado no próprio arquivo. Os demais
  cenários têm cobertura.
- Amostragem de traces é por razão fixa, sem tail sampling.
- O mapeamento `providerId ↔ cliente Keycloak` usa mappers hardcoded no
  realm de teste; em produção, grupos/roles do IdP.
- Credenciais LocalStack (test/test) e secrets de realm são de ambiente
  local, versionados de propósito para reprodutibilidade.
