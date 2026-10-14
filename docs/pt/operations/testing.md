> A suíte de testes existe em **quatro camadas**, cada uma com um
> propósito e um pré-requisito diferente. Esta página explica o que cada alvo faz,
> o que ele cobre e — mais importante — *o que ele prova que os outros não
> provam*.

## As quatro camadas

| Alvo | Pré-requisito | O que roda |
| :-- | :-- | :-- |
| `make test` | nenhum | `go test ./...` — suíte rápida, sem stack. |
| `make test-race` | nenhum | Igual, com `-race`. Detecta corrida de dados. |
| `make test-integration` | stack no ar | Testes com build tag `integration` contra Postgres, LocalStack e Keycloak reais. |
| `make e2e` | nenhum (sobe a stack) | `scripts/e2e.sh`: sobe, espera healthchecks, roda e derruba. |

```sh
# O dia a dia: rápido, sem infraestrutura.
make test

# Antes de fechar uma mudança de concorrência.
make test-race

# Com `make up` já de pé: prova contra os serviços reais.
make test-integration

# Prova de ponta a ponta, isolada: sobe e derruba tudo.
make e2e
```

> [!TIP]
> **Rode `make test` sempre; `make test-integration` quando o commit toca concorrência**
>
> O `make test` é a rede de segurança diária (dezenas de testes unitários,
> incluindo os que leem os arquivos de infraestrutura). O
> `make test-integration` é mais caro e mais lento, mas é o único que vê
> Postgres, SQS e Keycloak de verdade.


## A suíte rápida

- **399 testes de topo** (778 subtests com os table-driven) distribuídos entre domínio, serviço, adapters e
  platform.
- Roda **sem stack**: nenhum Postgres, nenhuma fila, nenhum Keycloak.
- Inclui o teste de **sincronia de infraestrutura** (`test/integration/tofu_sqs_sync_test.go`,
  sem build tag) que lê os dois arquivos declarativos e falha se divergirem —
  sem precisar do binário do tofu.

> [!NOTE]
> **O que `make test` deliberadamente não faz**
>
> Ele não sobe containers nem depende de rede. É o que o mantém rápido o
> suficiente para rodar a cada save. Correção de *concorrência* só aparece sob
> `-race` e sob Postgres real — daí `make test-race` e
> `make test-integration` existirem.


## `-race`

```sh
make test-race
```

O race detector encontra acessos concorrentes não sincronizados. O desenho usa
lock por linha + versão otimista, e é exatamente esse tipo de código que `-race`
audita. Rode-o sempre que a mudança encostar em goroutines, workers ou no
`TxManager`.

## Testes de integração

Requerem a stack de pé (`make up`) e são selecionados pela build tag
`integration`.

```sh
# Build tag `integration`; os endpoints vêm do ambiente WALLET_TEST_*.
go test -tags=integration ./test/integration/...
```

Variáveis usadas pelos testes de integração:

| Variável | Valor padrão |
| :-- | :-- |
| `WALLET_TEST_API_URL` | `http://localhost:8081` |
| `WALLET_TEST_DB_DSN` | `postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable` |
| `WALLET_TEST_KEYCLOAK` | `http://localhost:8080` |
| `WALLET_TEST_SQS_ENDPOINT` | `http://localhost:4566` |

> [!NOTE]
> **Dois grupos, dois bancos — e por quê**
>
> `make test-integration` separa a suíte em dois grupos, porque eles têm
> exigências diferentes de banco:
>
> 1. `./test/integration/...` testa **cruzando as pontas**: abre carteira
>    *pela API* e confere o resultado *no banco*. Precisa do banco da própria
>    stack (é onde a API escreve). Não force `WALLET_TEST_DB_DSN` para outro
>    banco aqui — todo `abre-e-lê-de-volta` falharia com resultado vazio, o que
>    é misconfiguração, não bug.
> 2. `./internal/postgres/...` e `./internal/transport/http/...` falam
>    **direto** com o banco e exercitam caminhos que deixam estado que as
>    invariantes rejeitariam (ex.: `UpdateBalance` sem o lançamento). Por isso
>    recebem `WALLET_TEST_DB_DSN` apontando para um banco **isolado e
>    descartável**, provisionado por `scripts/test-db.sh`.
>
> Sem essa separação, rodar `make test-integration` deixa resíduo no banco da
> stack e `make load-verify` acusa "stored balance equals ledger sum" etc. —
> um **falso** vermelho, resíduo de teste lido como corrupção de saldo.

> [!WARNING]
> **Uma `API_URL` em `8081`, não `8080`**
>
> Os testes de integração usam `http://localhost:8081` por padrão. Os
> containers expõem a API nessa porta (`make up-multi` em 8081/8082/8083), então
> apontar para `8080` — a porta do binário local — faz o teste falhar ou acabar
> batendo no processo errado.


### O que a integração cobre

| Área | O que prova |
| :-- | :-- |
| Concorrência / race | Apostas simultâneas na mesma carteira não corrompem saldo. |
| Idempotência | A mesma chave não debita duas vezes; chave distinta, corpo diferente conflita. |
| Partidas dobradas | Toda movimentação tem lançamento correspondente no ledger. |
| Kill-after-commit | Matar o processo após o commit não perde nem duplica o efeito. |
| Restart | Reiniciar retoma o consumo e o outbox sem repetir efeitos. |
| Resiliência | Dependência indisponível produz degradação, não corrupção. |
| Fluxo (flow) | O caminho completo de uma aposta, ponta a ponta. |
| Jornada (journey) | Sequência de operações como um player faria. |
| Mensageria | Envelope, atributos e roteamento das filas. |
| Outbox race | Dois publishers não publicam o mesmo evento duas vezes. |
| Reconcile worker | Divergência plantada é detectada pela varredura. |
| Continuidade de trace | O trace sobrevive à travessia da fila. |
| Políticas de Keycloak e SQS | As políticas declaradas são as efetivas. |
| Sincronia realm | O realm importado casa com o arquivo de realm. |
| Sincronia tofu/SQS | As declarações de fila no tofu e no boto não divergem. |

> [!TIP]
> **Testes que leem arquivos, não serviços**
>
> `realm_sync_test.go` e `tofu_sqs_sync_test.go` comparam **declarações** entre
> arquivos. Eles falham quando alguém edita um lado e esquece o outro — o tipo
> de divergência que nenhum teste de runtime pegaria.


## E2E

```sh
make e2e   # scripts/e2e.sh
```

O script sobe a stack, espera os healthchecks responderem e só então roda. **O
teardown derruba a stack** ao final — inclusive quando o teste passa.

> [!WARNING]
> **`make e2e` deixa você sem ambiente**
>
> Depois de um `make e2e`, rode `make up` de novo antes de qualquer trabalho
> interativo. Não é um bug: o e2e se propõe a ser hermético.


São ~25 testes e2e segundo o README, cobrindo o caminho real HTTP → SQS →
worker → Postgres.

## Escolhendo a camada certa

| Você mudou… | Rode |
| :-- | :-- |
| Regra de domínio, cálculo de saldo | `make test` |
| Goroutine, worker, `TxManager` | `make test`, depois `make test-race` |
| Query SQL, índice, transação | `make test-integration` |
| Envelope de evento, fila, política | `make test-integration` |
| Realm, tofu, script de infra | `make test` (testes de sincronia) |
| Fluxo de usuário de ponta a ponta | `make e2e` |

Veja também: [Teste de carga](load-testing.md) ·
[Estrutura do projeto](../contributing/project-structure.md) ·
[Solução de problemas](troubleshooting.md).
