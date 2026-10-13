> As convenções deste repositório não são estilo: elas codificam decisões de
> correção. `Money` em inteiro, IDs UUIDv7, erros de domínio tipados,
> transações via `TxManager` — cada uma existe porque a alternativa
> custa um bug. Esta página reúne as regras e o motivo de cada uma.

## Hexagonal na prática

A regra de dependência é a convenção mais antiga do projeto: **`domain` e
`service` não importam infraestrutura**.

| Camada | Pode importar | Não pode importar |
| :-- | :-- | :-- |
| `domain` | biblioteca padrão, `time`, `github.com/google/uuid` | `database/sql`, `net/http`, adapters |
| `service` | `domain`, pacotes de porta | `postgres`, `sqs`, `transport/http` |
| Adaptadores | `service` (portas), `domain` | regras de negócio próprias |

```go
// Certo: o serviço declara a porta; o adapter implementa.
type WalletRepository interface {
    GetForUpdate(ctx context.Context, id domain.WalletID) (*domain.Wallet, error)
}

// Errado: o serviço conhecendo pgx, o ORM, a tabela.
```

> [!TIP]
> **O teste de arquitetura**
>
> Se para adicionar uma regra de negócio você precisou importar um adapter
> dentro de `service`, a porta está faltando. Isso é um sinal de desenho, não um
> detalhe de import.


## `Money` é um value object em minor units

**Nunca use `float` para dinheiro.** `Money` é `int64` em unidades menores
(centavos) mais a moeda.

```go
// A forma: inteiro em unidades menores + moeda.
type Money struct {
    Amount   int64     // minor units: 1000 == 10,00
    Currency Currency  // ISO 4217 de 3 letras
}
```

| Regra | Por quê |
| :-- | :-- |
| `int64` em minor units | Ponto flutuante não representa `0.1` exatamente; somas de saldo acumulam erro. |
| Moeda junto do valor | Somar moedas diferentes é erro, não conversão implícita. |
| Overflow checado | `ErrMoneyOverflow` existe porque `int64` transborda silenciosamente. |
| Operações, não aritmética crua | `Add`, `Sub`, `Neg` validam antes de devolver. |

> [!WARNING]
> **`float64` é proibido na fronteira, não só no cálculo**
>
> Um único `float64` num codec de JSON ou num DTO já contamina o caminho:
> `10.00` entra, vira `9.999999999999998`, e o saldo fecha errado. Parseie direto
> para as unidades menores.


## IDs são UUIDv7

| Propriedade | Vantagem |
| :-- | :-- |
| Ordenado no tempo | Inserções vão para o **fim** do índice B-tree. |
| Globalmente único | Não há coordenação central para gerar. |
| Sem colisão entre réplicas | Três réplicas geram IDs sem se falar. |

> [!NOTE]
> **Por que não UUIDv4**
>
> UUIDv4 é aleatório, então cada `INSERT` cai num ponto aleatório do índice —
> e o índice sofre *page splits* constantes. UUIDv7 dá **localidade de índice**
> e mantém a ordenação por tempo que um `id` sequencial daria, sem o gargalo de
> um gerador central.


## Erros de domínio tipados

Todo erro de negócio é um `*domain.Error` com **`Code` estável** e `Message`
legível.

```go
// O código é contrato externo: corpo HTTP, failureCode na transação.
var ErrInsufficientFunds = &Error{Code: "INSUFFICIENT_FUNDS", Message: "..."}

// Adicione contexto sem perder a identidade para errors.Is.
return ErrInsufficientFunds.With("wallet %s has %s", id, bal)
```

| Regra | Por quê |
| :-- | :-- |
| `Code` nunca muda de significado | É contrato externo; dashboards e clientes dependem dele. |
| `With()` preserva identidade | `errors.Is(err, ErrSentinel)` continua valendo na cadeia. |
| Erros de infraestrutura ≠ erros de domínio | Só os de domínio viram `422`/`409`; os outros viram `5xx`. |

## Transações via `TxManager`

Toda escrita passa por `TxManager.WithinTx`. Repositórios obtidos fora de uma
transação **não são válidos** para escrita.

```go
// Uma transação, um efeito: ledger + inbox + outbox juntos.
err := tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
    if err := wallets.GetForUpdate(ctx, id); err != nil {
        return err
    }
    if err := ledger.Append(ctx, postings); err != nil {
        return err
    }
    return outbox.Enqueue(ctx, event)   // MESMA transação
})
```

> [!WARNING]
> **Repositório fora da transação é um erro de programação**
>
> Não é uma restrição artificial: o lock por linha, a versão otimista e a
> atomicidade do outbox só existem **dentro** de uma transação. Usar o
> repositório por fora descarta exatamente a garantia que o desenho compra.


## Idempotência e chaves

A chave de idempotência é única por operação e comparada junto do corpo.

| Caso | Resultado |
| :-- | :-- |
| Mesma chave, mesmo corpo | Replay idempotente — mesma resposta, sem novo efeito. |
| Mesma chave, corpo diferente | `409 IDEMPOTENCY_KEY_CONFLICT`. |
| Chave nova | Processamento normal. |

> [!NOTE]
> **Rejeições também são persistidas**
>
> Uma rejeição (`422`) grava o registro da tentativa. O dedup não protege só o
> caminho feliz — ele cobre a repetição da mesma rejeição.


## Comentários e commits em português

- **Comentários de código**: em português, explicando *por que*, não *o quê*.
- **Mensagens de commit**: em português.
- **Documentação**: este site é bilíngue (PT/EN), espelhado.

> [!TIP]
> **Comente a decisão, não a linha**
>
> `// incrementa o contador` não ajuda ninguém. `// o lock da linha garante que
> duas apostas concorrentes não leiam o mesmo saldo` é o que salva o próximo
> leitor.


## Testes de integração têm build tag

Testes que dependem de Postgres, SQS ou Keycloak ficam sob a tag `integration` e
só rodam em `make test-integration`.

```go
//go:build integration

package integration
```

| Regra | Por quê |
| :-- | :-- |
| Tag `integration` para testes com stack | `make test` continua rápido e offline. |
| Testes de sincronia sem tag | Leem arquivos; rodam sempre, custam nada. |
| Nomes de teste descritivos | O nome é o que aparece na falha do CI. |

## Checklist antes de abrir um PR

| Item | Onde |
| :-- | :-- |
| Dependência aponta para dentro | `internal/domain`, `internal/service` |
| Dinheiro nunca é `float` | `internal/domain/money.go` |
| IDs são UUIDv7 | `internal/domain/ids.go` |
| Erros de negócio com `Code` estável | `internal/domain/errors.go` |
| Escrita dentro de `WithinTx` | `internal/postgres/tx_repo.go` |
| Métrica nova documentada | `internal/platform/metrics` + dashboard |
| Variável nova em `config` | `internal/platform/config/config.go` |
| Filas sincronizadas (boto + tofu) | `deploy/localstack/init` + `deploy/tofu` |
| `make test` verde | — |

Veja também: [Decisões técnicas](decisions.md) ·
[Estrutura do projeto](project-structure.md) ·
[Testes](../operations/testing.md).
