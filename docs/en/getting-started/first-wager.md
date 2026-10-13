> Your first wager, from scratch: a *provider* token, a `BET`
> with an `Idempotency-Key`, reading the response and — the part that
> matters — the proof that repeating the same key returns the same result without
> moving money twice.

## Prerequisites

This page assumes the stack is up (`make up`) and a wallet has been opened. If
you have not done that yet, start with the [Quickstart](quickstart.md).

```sh
API=http://localhost:8081
PLAYER=0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1

# internal token: opens and reads wallets
TOKEN_INT=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=internal-secret | jq -r .access_token)

# provider token: submits wagers
TOKEN_A=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret | jq -r .access_token)

# open the wallet (opening balance) and keep the id
WALLET=$(curl -s -X POST $API/wallets \
  -H "Authorization: Bearer $TOKEN_INT" -H 'Content-Type: application/json' \
  -d '{"playerId":"'$PLAYER'","initialBalance":{"amount":"1000.00","currency":"BRL"}}' \
  | jq -r .id)
echo "wallet = $WALLET"
```

> [!NOTE]
> **Why two tokens**
>
> `wallet-internal` (scope `wallet:internal`) opens and reads wallets. A
> **wager**, on the other hand, requires a **provider** token (`provider-a`,
> scope `wallet:provider`): the service derives the `providerId` from the
> signed claims and refuses to act on behalf of another provider.


## The shape of a wager

```mermaid
sequenceDiagram
    autonumber
    participant P as Provider (you)
    participant KC as Keycloak
    participant API as HTTP (go-kit)
    participant DB as PostgreSQL

    P->>KC: POST /token (client_credentials, provider-a)
    KC-->>P: access_token (wallet:provider)
    P->>API: POST /wagering/transactions  
Bearer + Idempotency-Key
    API->>API: validate JWT (JWKS) and authorise provider-a
    API->>DB: BEGIN · FOR UPDATE · INSERT · UPDATE · COMMIT
    DB-->>API: updated balance
    API-->>P: 200 { transactionId, status: PROCESSED, balance }

    Note over P,API: REPLAY — same key, same body
    P->>API: POST /wagering/transactions  
same Idempotency-Key
    API-->>P: identical 200 · idempotentReplay: true  
(no new ledger entry)
```

## The first BET

A wager is a `POST /wagering/transactions` with a **mandatory**
`Idempotency-Key`:

```sh
curl -s -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $TOKEN_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "'$PLAYER'",
    "walletId": "'$WALLET'",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }' | jq
```

Response:

```json
{
  "transactionId": "0192f28f-7b12-7a44-bd10-3f9c1e2a4b55",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

| Field | What it means |
| :-- | :-- |
| `transactionId` | **internal** id of the operation (not the `externalTransactionId`) |
| `status` | `PROCESSED` once the money has moved |
| `balance` | wallet balance **after** the operation |
| `idempotentReplay` | `false` on the first run, `true` on a replay |

`money` is always a **decimal string + currency** — never a `float`. The service
stores it in minor units (`int64`) and returns the same representation.

> [!NOTE]
> **The kinds**
>
> `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`. `LOSS` is **informational**
> (`amount = 0`): it records that the round paid no prize, without moving the
> balance. `REFUND`/`ROLLBACK` require a `referenceExternalTransactionId`
> pointing at an operation from the **same** `roundId`, player, wallet and
> currency.


## The REPLAY: repeating moves money only once

Repeat **exactly** the same request — same key, same body:

```sh
curl -s -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $TOKEN_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "'$PLAYER'",
    "walletId": "'$WALLET'",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }' | jq
```

The response is the **same**, except for one field:

```json
{
  "transactionId": "0192f28f-7b12-7a44-bd10-3f9c1e2a4b55",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": true
}
```

Same `transactionId`, same balance (`975.00`, **not** `950.00`),
`"idempotentReplay": true`. The balance did not move because the ledger entry
was not repeated.

> [!TIP]
> **Prove it yourself**
>
> Check the balance after the replay — it is still `975.00`:
> ```sh
> curl -s $API/wallets/$WALLET -H "Authorization: Bearer $TOKEN_INT" | jq .balance
> ```


## The conflict: same key, different body

The identity of an idempotent call is the key **plus** the content. Reusing the
key with a different body is an error, and the service answers `409`:

```sh
# same Idempotency-Key, but a different amount
curl -s -o /dev/null -w '%{http_code}\n' -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $TOKEN_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "playerId": "'$PLAYER'",
    "walletId": "'$WALLET'",
    "roundId": "round-987", "gameId": "fortune-chimp", "kind": "BET",
    "money": { "amount": "99.00", "currency": "BRL" }
  }'
# => 409
```

```json
{
  "error": {
    "code": "IDEMPOTENCY_KEY_CONFLICT",
    "message": "idempotency key already used with a different payload"
  }
}
```

| Scenario | Status | Code |
| :-- | :-- | :-- |
| Same key, same body | `200` | — (`idempotentReplay: true`) |
| Same key, different body | `409` | `IDEMPOTENCY_KEY_CONFLICT` |
| `externalTransactionId` already used differently | `409` | `EXTERNAL_TRANSACTION_ID_CONFLICT` |
| Insufficient balance | `422` | `INSUFFICIENT_FUNDS` |
| Mismatched reference | `422` | `REFERENCE_MISMATCH` |

> [!WARNING]
> **The identical response is the guarantee, not a detail**
>
> A network retry that happens to hit two replicas must not debit twice. The
> result of the first run is **persisted** and replayed: that is why the
> response is byte-for-byte identical, and only the `idempotentReplay` flag
> changes.


## Read the ledger

The balance is a *derived cache*; the truth lives in the **append-only** ledger.
Every movement is an immutable row:

```sh
curl -s "$API/wallets/$WALLET/ledger?limit=50" \
  -H "Authorization: Bearer $TOKEN_INT" | jq
```

```json
{
  "items": [
    {
      "id": "0192f28f-6a10-7001-8000-000000000001",
      "transactionId": "0192f28f-6a00-7001-8000-000000000000",
      "direction": "CREDIT",
      "amount": { "amount": "1000.00", "currency": "BRL" },
      "balanceBefore": { "amount": "0.00", "currency": "BRL" },
      "balanceAfter": { "amount": "1000.00", "currency": "BRL" },
      "createdAt": "2026-09-08T12:00:00Z"
    },
    {
      "id": "0192f28f-7b12-7001-8000-000000000002",
      "transactionId": "0192f28f-7b12-7a44-bd10-3f9c1e2a4b55",
      "direction": "DEBIT",
      "amount": { "amount": "25.00", "currency": "BRL" },
      "balanceBefore": { "amount": "1000.00", "currency": "BRL" },
      "balanceAfter": { "amount": "975.00", "currency": "BRL" },
      "createdAt": "2026-09-08T12:00:05Z"
    }
  ]
}
```

There are **two** entries for **one** wager — the opening did not duplicate (the
replay inserted nothing) and the BET debited `25.00`.

Confirm the balance is the sum of the entries:

```sh
curl -s -X POST $API/wallets/$WALLET/reconciliation \
  -H "Authorization: Bearer $TOKEN_INT" | jq
```

```json
{
  "walletId": "0192f28f-6a01-7c33-9c21-8b7d2e6f0a19",
  "storedBalance": { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

`"consistent": true` and a `difference` of zero mean the balance matches the
ledger.

See also: [Quickstart](quickstart.md) · [Five minutes](five-minutes.md) ·
[Idempotency](../api/idempotency.md) · [Ledger](../data/ledger.md).
