> O contrato de erro do serviço: um único envelope JSON, uma tabela de mapeamento
> para HTTP construída por *classificação* — não por `if` de
> string — e o catálogo completo de códigos estáveis de domínio.

## O envelope

Toda falha sai com o mesmo corpo. O `code` é estável e é a única coisa em que
um cliente deve ramificar; a `message` é detalhe humano e **não é contrato**.

```json
{
  "error": {
    "code": "INSUFFICIENT_FUNDS",
    "message": "insufficient funds"
  }
}
```

> [!TIP]
> **Ramifique pelo `code`, nunca pela `message`**
>
> A `message` pode ganhar contexto sem aviso. O `code` é versionado e
> documentado — é ele que pertence ao seu `switch`.


## Erro → HTTP

O encoder classifica o erro por tipo (`ValidationError`, `NotFoundError`,
`ConflictError`, `UnavailableError`) e escolhe o status. Isso mantém o
mapeamento **determinístico** e independente da mensagem.

| HTTP | Código | Quando |
| :-- | :-- | :-- |
| ❌ `400` | `VALIDATION_FAILED` | Entrada malformada; **nunca persistida** |
| ❌ `401` | `UNAUTHORIZED` | Credencial ausente, inválida ou expirada |
| ❌ `403` | `FORBIDDEN` | Autenticado, mas sem permissão |
| ❌ `404` | `*_NOT_FOUND` | Recurso não existe (ex.: `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND`) |
| ❌ `409` | `IDEMPOTENCY_KEY_CONFLICT` · `EXTERNAL_TRANSACTION_ID_CONFLICT` · `WALLET_ALREADY_EXISTS` | Conflito de identidade |
| ❌ `422` | (código de domínio no `failureCode`) | Rejeição de negócio **persistida** (status `REJECTED`) |
| ❌ `503` | `TRANSIENT_UNAVAILABLE` | PostgreSQL/SQS temporariamente indisponíveis |
| ❌ `500` | `INTERNAL_ERROR` | Bug ou falha desconhecida |

> [!WARNING]
> **400 e 422 são coisas diferentes**
>
> **400** significa que o corpo nem chegou ao domínio — nada foi
> gravado. **422** significa o oposto: a rejeição de negócio foi
> **persistida** com status `REJECTED`, e o corpo
> carrega `failureCode`/`failureMessage`. Reenviar a
> mesma chave de idempotência devolve o mesmo `422`.


## Códigos de domínio

Estes códigos vêm de `internal/domain/errors.go` e são parte do contrato
externo: aparecem como `code` (quando o erro sobe sozinho) ou como
`failureCode` (quando a transação é persistida como `REJECTED`).

| Código | Mensagem | Significado |
| :-- | :-- | :-- |
| `INVALID_MONEY` | invalid money value | Valor monetário ilegível/inválido |
| `MONEY_OVERFLOW` | money amount out of supported range | Estoura a faixa representável |
| `INVALID_CURRENCY` | currency must be a 3-letter ISO 4217 code | Moeda fora do formato ISO 4217 |
| `CURRENCY_MISMATCH` | operation currency differs from wallet currency | Moeda da operação ≠ moeda da carteira |
| `INVALID_AMOUNT` | amount violates the operation value policy | Valor viola a política (ex.: negativo) |
| `INVALID_KIND` | unknown transaction kind | `kind` desconhecido |
| `KIND_NOT_ALLOWED` | operation kind not allowed on this channel | `kind` não permitido no canal |
| `INSUFFICIENT_FUNDS` | insufficient funds | Saldo insuficiente para o débito |
| `INVALID_STATE_TRANSITION` | invalid transaction state transition | Transição de estado proibida |
| `WALLET_ALREADY_EXISTS` | wallet already exists for player and currency | Já existe carteira `(jogador, moeda)` |
| `LEDGER_ARITHMETIC` | ledger entry does not balance | Lançamento não fecha |
| `INVALID_TRANSACTION` | transaction payload is invalid | Payload de transação inválido |
| `REFERENCE_REQUIRED` | referenceExternalTransactionId is required for REFUND/ROLLBACK | Falta a referência exigida |
| `INVALID_WALLET_VERSION` | wallet version must start at 1 | Versão inicial deve ser 1 |
| `INVALID_ACCOUNT` | account is not a valid chart-of-accounts entry | Conta fora do plano de contas |
| `UNBALANCED_JOURNAL` | journal entry debits do not equal credits | Débitos ≠ créditos no lançamento |

### Códigos de transporte

Definidos em `internal/service/errors.go`, acompanham a classificação HTTP.

| Código | HTTP | Origem |
| :-- | :-- | :-- |
| `VALIDATION_FAILED` | ❌ `400` | `ValidationError` |
| `NOT_FOUND` | ❌ `404` | `NotFoundError` genérico |
| `WALLET_NOT_FOUND` | ❌ `404` | `NotFoundError` com código específico |
| `TRANSACTION_NOT_FOUND` | ❌ `404` | `NotFoundError` com código específico |
| `IDEMPOTENCY_KEY_CONFLICT` | ❌ `409` | `ConflictError` |
| `EXTERNAL_TRANSACTION_ID_CONFLICT` | ❌ `409` | `ConflictError` |
| `WALLET_ALREADY_EXISTS` | ❌ `409` | `ConflictError` |
| `PROVIDER_MISMATCH` | ❌ `409` | `ConflictError` |
| `TRANSIENT_UNAVAILABLE` | ❌ `503` | `UnavailableError` |
| `UNAUTHORIZED` | ❌ `401` | identidade ausente/inválida |
| `FORBIDDEN` | ❌ `403` | identidade sem permissão |

> [!NOTE]
> **`FailureCode` faz a ponte**
>
> Todo erro — de domínio ou não — é reduzido a um código estável. É esse
> código que vira `failureCode` no corpo de uma rejeição persistida, o que
> permite ao provedor entender a recusa sem depender do texto.


Veja também: [Referência da API](reference.md) · [Idempotência](idempotency.md) ·
[Exemplos](examples.md).
