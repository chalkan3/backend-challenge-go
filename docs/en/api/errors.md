> The service's error contract: a single JSON envelope, a mapping table to HTTP
> built by *classification* — not by string `if`s — and the
> complete catalogue of stable domain codes.

## The envelope

Every failure comes out with the same body. The `code` is stable and is the
only thing a client should branch on; the `message` is human detail and is
**not** a contract.

```json
{
  "error": {
    "code": "INSUFFICIENT_FUNDS",
    "message": "insufficient funds"
  }
}
```

> [!TIP]
> **Branch on the `code`, never on the `message`**
>
> The `message` may gain context without notice. The `code` is versioned and
> documented — it is the one that belongs in your `switch`.


## Error → HTTP

The encoder classifies the error by type (`ValidationError`, `NotFoundError`,
`ConflictError`, `UnavailableError`) and picks the status. This keeps the
mapping **deterministic** and independent of the message.

| HTTP | Code | When |
| :-- | :-- | :-- |
| ❌ `400` | `VALIDATION_FAILED` | Malformed input; **never persisted** |
| ❌ `401` | `UNAUTHORIZED` | Missing, invalid or expired credential |
| ❌ `403` | `FORBIDDEN` | Authenticated but not allowed |
| ❌ `404` | `*_NOT_FOUND` | Resource does not exist (e.g. `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND`) |
| ❌ `409` | `IDEMPOTENCY_KEY_CONFLICT` · `EXTERNAL_TRANSACTION_ID_CONFLICT` · `WALLET_ALREADY_EXISTS` | Identity conflict |
| ❌ `422` | (domain code in `failureCode`) | **Persisted** business rejection (status `REJECTED`) |
| ❌ `503` | `TRANSIENT_UNAVAILABLE` | PostgreSQL/SQS temporarily unavailable |
| ❌ `500` | `INTERNAL_ERROR` | Bug or unknown failure |

> [!WARNING]
> **400 and 422 are not the same thing**
>
> **400** means the body never reached the domain — nothing was
> written. **422** means the opposite: the business rejection was
> **persisted** with status `REJECTED`, and the body
> carries `failureCode`/`failureMessage`. Resending the
> same idempotency key returns the same `422`.


## Domain codes

These codes come from `internal/domain/errors.go` and are part of the external
contract: they show up as `code` (when the error surfaces on its own) or as
`failureCode` (when the transaction is persisted as `REJECTED`).

| Code | Message | Meaning |
| :-- | :-- | :-- |
| `INVALID_MONEY` | invalid money value | Unreadable/invalid monetary value |
| `MONEY_OVERFLOW` | money amount out of supported range | Overflows the representable range |
| `INVALID_CURRENCY` | currency must be a 3-letter ISO 4217 code | Currency outside ISO 4217 format |
| `CURRENCY_MISMATCH` | operation currency differs from wallet currency | Operation currency ≠ wallet currency |
| `INVALID_AMOUNT` | amount violates the operation value policy | Amount violates the policy (e.g. negative) |
| `INVALID_KIND` | unknown transaction kind | Unknown `kind` |
| `KIND_NOT_ALLOWED` | operation kind not allowed on this channel | `kind` not allowed on the channel |
| `INSUFFICIENT_FUNDS` | insufficient funds | Balance too low for the debit |
| `INVALID_STATE_TRANSITION` | invalid transaction state transition | Forbidden state transition |
| `WALLET_ALREADY_EXISTS` | wallet already exists for player and currency | A `(player, currency)` wallet already exists |
| `LEDGER_ARITHMETIC` | ledger entry does not balance | Entry does not balance |
| `INVALID_TRANSACTION` | transaction payload is invalid | Invalid transaction payload |
| `REFERENCE_REQUIRED` | referenceExternalTransactionId is required for REFUND/ROLLBACK | The required reference is missing |
| `INVALID_WALLET_VERSION` | wallet version must start at 1 | Initial version must be 1 |
| `INVALID_ACCOUNT` | account is not a valid chart-of-accounts entry | Account outside the chart of accounts |
| `UNBALANCED_JOURNAL` | journal entry debits do not equal credits | Debits ≠ credits in the entry |

### Transport codes

Defined in `internal/service/errors.go`, they accompany the HTTP classification.

| Code | HTTP | Origin |
| :-- | :-- | :-- |
| `VALIDATION_FAILED` | ❌ `400` | `ValidationError` |
| `NOT_FOUND` | ❌ `404` | generic `NotFoundError` |
| `WALLET_NOT_FOUND` | ❌ `404` | `NotFoundError` with a specific code |
| `TRANSACTION_NOT_FOUND` | ❌ `404` | `NotFoundError` with a specific code |
| `IDEMPOTENCY_KEY_CONFLICT` | ❌ `409` | `ConflictError` |
| `EXTERNAL_TRANSACTION_ID_CONFLICT` | ❌ `409` | `ConflictError` |
| `WALLET_ALREADY_EXISTS` | ❌ `409` | `ConflictError` |
| `PROVIDER_MISMATCH` | ❌ `409` | `ConflictError` |
| `TRANSIENT_UNAVAILABLE` | ❌ `503` | `UnavailableError` |
| `UNAUTHORIZED` | ❌ `401` | missing/invalid identity |
| `FORBIDDEN` | ❌ `403` | identity without permission |

> [!NOTE]
> **`FailureCode` is the bridge**
>
> Every error — domain or not — is reduced to a stable code. That code is
> what becomes `failureCode` in a persisted rejection's body, letting the
> provider understand the refusal without depending on the text.


See also: [API reference](reference.md) · [Idempotency](idempotency.md) ·
[Examples](examples.md).
