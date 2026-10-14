# Testes de carga

Testes de carga para o wallet, escritos em [k6](https://k6.io) e organizados em
torno de uma ideia: **carga sem invariante não prova nada**. Um número de
throughput bonito não distingue um serviço correto de um que perde atualizações
com elegância.

O k6 roda a partir da imagem oficial (`grafana/k6`), não é instalado na máquina —
o mesmo critério que o resto do projeto usa para compose e kind. Este diretório
separa duas metades que só juntas formam a alegação:

| Camada | Arquivo | O que ela prova |
| :-- | :-- | :-- |
| **Carga + contrato** | `k6/scenarios/*.js` | que a API *responde* certo, sob concorrência |
| **Integridade** | `sql/invariants.sql` | que o que foi *commitado* é consistente |

O k6 não vê o Postgres. As invariantes contábeis — saldo igual à soma do ledger,
ausência de saldo negativo, chave de idempotência única — só existem na segunda
camada. É por isso que `make load-verify` é um alvo separado e não um passo
escondido no fim dos cenários.

---

## Cenários

Cada cenário ataca uma garantia específica do desafio.

### `smoke.js` — o contrato inteiro, rápido

Um VU, três iterações, mas percorre a API toda: health, abertura com
`Idempotency-Key`, aposta, **replay**, **conflito** (mesma chave, outro payload),
saque por saldo insuficiente, leitura do ledger e reconciliação. É o que roda em
CI: um contrato quebrado falha em segundos, antes dos cenários caros.

### `hot-wallet.js` — o lock, no pior caso

**Uma** carteira, N VUs, todas as escritas serializando na mesma linha. É o
cenário que expõe *lost update*: se o lock não segurasse, duas operações
concorrentes leriam o mesmo `balance_before` e o saldo final estaria errado.

O contrato é aritmético de propósito, para o resultado ser verificável: cada
operação aplicada move exatamente um valor, BET debita, WIN credita, o seed cobre
todos os débitos. No `teardown`, portanto:

```
saldo   == seed - débitos + créditos      (ao centavo)
version == operações aplicadas
ledger_entries == operações + 1 (a abertura)
reconciliação do próprio serviço == consistente
```

Se qualquer uma falhar, o k6 encerra com erro. Não é um check de status HTTP:
é uma asserção sobre o estado do banco.

### `idempotency.js` — as três cláusulas da idempotência

1. **mesma chave + mesmo payload** → replay do resultado original, dinheiro move
   **uma** vez. Testado sob concorrência real, não em sequência: várias VUs
   submetem a mesma chave no mesmo instante, exercitando a corrida dentro da
   transação.
2. **mesma chave + payload diferente** → `409 IDEMPOTENCY_KEY_CONFLICT`
   (verificado explicitamente no `teardown`).
3. **chaves distintas, mesmo payload** → aplica duas vezes. Este é o controle que
   pega o bug sutil de deduplicar por *hash do payload* em vez de pela chave —
   nesse caso o teste falha com "the service is deduplicating on payload, not on
   the key".

### `mixed.js` — tráfego de produção

Muitas carteiras independentes, vários tipos de operação, e uma fração
(`BAD_FRACTION`) de requisições que **devem** ser recusadas. Responde duas
perguntas que os outros cenários não respondem: o throughput escala com carteiras
independentes, ou o lock por linha virou um gargalo global? E o serviço recusa o
que deve recusar, com o código certo, sob carga?

As linhas adversariais, e o desfecho **real e verificado** de cada uma:

| Requisição | Resposta esperada |
| :-- | :-- |
| BET maior que o saldo | `422 INSUFFICIENT_FUNDS` |
| REFUND com referência inexistente | `202 PENDING_REFERENCE` (ver nota) |
| Carteira desconhecida | `404 WALLET_NOT_FOUND` |
| Segunda carteira para o mesmo jogador | `409 WALLET_ALREADY_EXISTS` |

> [!NOTE]
> **REFUND com referência desconhecida não é erro.** O serviço responde
> `202 PENDING_REFERENCE` — a referência é tratada como obrigação futura que o
> *reference resolver* vai retentar, não como requisição inválida. O cenário fixa
> esse comportamento: um `422` ali significaria que o caminho de resolução foi
> perdido.

No `teardown`, uma amostra das carteiras é reconciliada uma a uma: o lock protege
cada carteira, e a reconciliação confirma que o ledger continua somando ao saldo
depois do tráfego misto.

### `distributed.js` — o lock **entre processos**

Aponta para várias réplicas ao mesmo tempo e mantém a mesma carteira quente. A
invariante é a que importa:

> O lock por linha precisa valer **entre processos**, não só dentro de um. Se ele
> fosse um mutex de Go ou um cache em processo, este cenário corromperia o saldo
> enquanto o cenário de instância única continuaria verde.

O `teardown` lê o saldo por **todas** as réplicas e falha se alguma divergir, e
depois aplica a mesma aritmética do `hot-wallet`. Verde aqui significa: as N
instâncias concordam no saldo e o resultado é o mesmo que o de uma instância só.

Requer a topologia multi: `make up-multi` (portas `:8081/:8082/:8083`).

### `soak.js` — sustentado

Carga contínua (`constant-vus`) sobre muitas carteiras. Responde uma pergunta
diferente dos bursts: algo **deriva**? Observa três coisas: latência ao longo do
tempo (vazamento aparece no p95 tardio, não na média), correção ao final (cada
carteira reconciliada) e o backlog do outbox (`soak_outbox_pending`), que crescer
sem parar indica que o relay perdeu a corrida.

```sh
make load-soak DURATION=30m VUS=32
```

---

## Verificação de integridade (`sql/invariants.sql`)

Roda direto no Postgres, sem serviço no meio. Cada auditoria imprime

```
AUDIT|<n>|<descrição>|<valor>
```

e passa somente com `<valor> == 0`. Linhas `INFO|` são reportadas e não falham.

| # | Invariante | O que uma violação significa |
| :- | :-- | :-- |
| 1 | Cadeia do ledger intacta | `balance_before` ≠ `balance_after` anterior → histórico reescrito |
| 2 | Valor bate com a direção | entrada que não move o que diz mover |
| 3 | Nenhum saldo negativo | o `CHECK` do schema foi contornado |
| 4 | Saldo = soma do ledger | a fonte da verdade divergiu do agregado |
| 5 | `version` = nº de entradas | contador de operações perdendo eventos |
| 6 | Uma carteira por (jogador, moeda) | unicidade do schema violada |
| 7 | Chaves de idempotência únicas | replay virou débito duplo |
| 8 | PROCESSED não-nulo tem lançamento | dinheiro movido sem rastro |
| 9 | REJECTED não moveu dinheiro | recusa que debitou mesmo assim |
| 10 | Backlog do outbox | *informacional* — ver abaixo |
| 11 | Journal fecha (créditos == débitos por transação) | uma transação criou ou destruiu valor |
| 11b | Todo movimento processado tem posting | ledger com rastro, journal sem contraparte |
| 11c | Perna do jogador == linha do ledger | journal divergiu do dinheiro que moveu |

> [!NOTE]
> **Backlog do outbox é informativo, não falha.** Depois de rodar os cenários, o
> backlog fica na casa das centenas de milhares. Isso é **esperado** neste teste:
> os cenários produzem a ~2.600 requisições/s enquanto o relay publica a ~200
> eventos/s. Não é um bug do serviço — é a taxa de produção do gerador superando
> a de publicação do relay. O número está ali para ser lido, não para reprovar o
> run; um relay quebrado apareceria como backlog que **não para de crescer** com
> o sistema ocioso.

---

## Como rodar

```sh
make up                    # a stack, em uma instância
make load-smoke            # contrato completo, segundos
make load                  # hot + idempotency + mixed
make load-verify           # auditoria de integridade no Postgres

make up-multi              # três réplicas (:8081/:8082/:8083)
make load-distributed      # o lock entre processos
```

Alvos individuais: `load-hot`, `load-idempotency`, `load-mixed`, `load-soak`,
`load-distributed`, `load-verify`.

Parâmetros são repassados como argumentos do make:

```sh
make load-hot VUS=128 ITER_PER_VU=20
make load-soak DURATION=30m VUS=32
make load-distributed APIS=http://localhost:8081,http://localhost:8082
```

Para chamar o k6 direto (sem make):

```sh
scripts/load-run.sh smoke
scripts/load-run.sh hot VUS=48
API=http://localhost:8084 scripts/load-run.sh smoke   # topologia split, papel api
APIS=http://localhost:8081,http://localhost:8082 scripts/load-run.sh distributed
```

Sobrescritas de ambiente lidas por `k6/lib/config.js`: `API`, `APIS`, `KEYCLOAK`,
`REALM`, `CURRENCY`, `AMOUNT`, `GAME_ID`, `BAD_FRACTION`, `OVERDRAW_AMOUNT`.

---

## Calibração dos thresholds

Um threshold que sempre passa é documentação, não teste. Mas um envelope só
significa alguma coisa **em relação à máquina que produziu o número** — e esta
seção ignorava isso, o que fazia o CI reprovar todo push.

Os envelopes `local` foram medidos nesta máquina (Podman, compose, uma instância
combinada) e depois afrouxados em ~50% — uma regressão real dispara, a variância
normal não.

| Cenário | p50 | p95 | envelope `local` |
| :-- | --: | --: | :-- |
| `hot-wallet` | 70ms | 76ms | `p95 < 200ms` |
| `distributed` | 117ms | 310ms | `p95 < 600ms` |
| `mixed` | 11ms | 32ms | `p95 < 100ms` |

### Por que existem dois perfis

O mesmo cenário, medido no runner do GitHub, deu p95 de **297ms** contra os 76ms
daqui — ~3,9x. Com o envelope de 200ms, o job `load` reprovava **todo** push, não
por regressão do serviço, mas porque o envelope havia sido calibrado em outra
máquina. Um vermelho que nenhuma mudança de código conserta ensina as pessoas a
ignorarem vermelho.

Então os dois tipos de threshold foram separados pelo que de fato afirmam:

| | Muda com o ambiente? | O que reprova |
| :-- | :-- | :-- |
| **Correção** (`checks`, `http_req_failed`) | **Não** — absoluto e idêntico em toda máquina | um único 5xx, uma atualização perdida, um débito duplicado |
| **Latência** (`wallet_money_latency`, `wallet_read_latency`) | **Sim** — é orçamento, e o orçamento depende da máquina | uma regressão além do envelope do perfil |

O perfil é escolhido por `WALLET_LATENCY_PROFILE`, com default `local`:

| Perfil | Onde | Envelope (hot-wallet) | Pega |
| :-- | :-- | :-- | :-- |
| `local` | `make load-hot` | `p95 < 200ms` | regressões pequenas, enquanto se desenvolve |
| `ci` | job `load` do CI | `p95 < 2000ms` | regressões de ordem de grandeza |

O default é `local` porque é a escolha conservadora: um envelope apertado falha
alto, e uma falha falsa durante o desenvolvimento é barata de investigar. O CI
**opta** por `ci` explicitamente no workflow, para o afrouxamento aparecer no
diff em vez de ficar escondido no arquivo de thresholds.

Um valor desconhecido em `WALLET_LATENCY_PROFILE` **falha imediatamente** em vez
de cair num default: um typo não pode alargar o gate em silêncio.

```sh
make load-hot                                  # perfil local, apertado
make load-hot WALLET_LATENCY_PROFILE=ci        # como o CI roda
```

### Overlap: o envelope é do shape, não da máquina

Dentro de um perfil, os envelopes **não são o mesmo número, de propósito**. Os
cenários quente e distribuído serializam todas as escritas em uma linha: a
latência ali é fila, e cresce com o número de VUs. Medir isso contra o número do
tráfego misto seria ou falha permanente, ou carimbo. O `mixed`, que espalha por
carteiras independentes, é o que reflete a latência de produção em regime.

O único threshold absoluto é `checks: rate==1` e `http_req_failed: rate==0`:
correção não tem orçamento de erro. Um único 5xx, uma única atualização perdida
ou um único débito duplicado reprovam o run, por mais rápida que a resposta tenha
sido — e isso vale nos dois perfis.

---

## O que este diretório **não** cobre

Vale ser explícito, para o resultado não ser lido como mais do que é:

- **Não é um benchmark.** Os números valem para esta máquina; não são uma
  afirmação de capacidade em produção.
- **Não testa o consumidor SQS.** Os cenários usam HTTP. O caminho `SQS →
  consumer → inbox` (dedup por mensagem) é coberto pelos testes de integração em
  `test/integration`, não aqui.
- **Não prova o relay do outbox.** O cenário `soak` observa o backlog, mas não
  força o relay a falhar no meio da publicação — para isso existe
  `internal/messaging/relay_test.go` com um publisher que falha por design.
