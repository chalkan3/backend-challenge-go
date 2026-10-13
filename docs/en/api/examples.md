> Complete, copy-pasteable `curl` recipes, in the order a provider runs
> them: get a token, open a wallet, bet, query, reconcile — and deliberately
> trigger the 409, the 422 and the 202 to see each path of the contract.

## Environment variables

Set these once; the blocks below use them.

```sh
API=http://localhost:8080
KEYCLOAK=http://localhost:8080/realms/wallet/protocol/openid-connect/token

# Identities available in the dev realm (deploy/keycloak).
INTERNAL_ID=wallet-internal
INTERNAL_SECRET=internal-secret
PROVIDER_ID=provider-a
PROVIDER_SECRET=provider-a-secret
```

> [!TIP]
> **Check the binary and the compose**
>
> `make up` brings up the full stack. Business routes only exist outside
> worker mode; under `WALLET_MODE=worker` they return
> ❌ `404` on purpose.


## 1. Get a token from Keycloak

The service validates the JWT via JWKS and requires the `wallet-api` audience.
Each client is born with the right scope (`wallet:internal` or
`wallet:provider`).

```sh
TOKEN=$(curl -s -X POST "$KEYCLOAK" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=$PROVIDER_ID" \
  -d "client_secret=$PROVIDER_SECRET" \
  | jq -r .access_token)

echo "$TOKEN"
```

For internal routes, repeat with the internal identity:

```sh
TOKEN_INT=$(curl -s -X POST "$KEYCLOAK" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=$INTERNAL_ID" \
  -d "client_secret=$INTERNAL_SECRET" \
  | jq -r .access_token)
```

> [!WARNING]
> **No token, no conversation**
>
> Any business route without a valid `Authorization` returns
> ❌ `401` `UNAUTHORIZED`. A
> token for another audience is refused the same way.


## 2. Open a wallet

`POST` **`/wallets`** — internal identity.

```sh
PLAYER_ID=$(uuidgen)

curl -s -X POST "$API/wallets" \
  -H "Authorization: Bearer $TOKEN_INT" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: open-$PLAYER_ID-BRL" \
  -d "{
    \"playerId\": \"$PLAYER_ID\",
    \"initialBalance\": { \"amount\": \"1000.00\", \"currency\": \"BRL\" }
  }" | tee /tmp/wallet.json

WALLET_ID=$(jq -r .id /tmp/wallet.json)
echo "walletId=$WALLET_ID"
```

Response (✅ `201`):

```json
{
  "id": "8a2f5c1e-77b9-4a11-9d02-0e1f2a3b4c5d",
  "playerId": "3f1c4b2e-0a9d-4d7e-9b11-2c3d4e5f6a70",
  "balance": { "amount": "1000.00", "currency": "BRL" },
  "version": 1,
  "createdAt": "2024-05-01T12:00:00Z",
  "updatedAt": "2024-05-01T12:00:00Z"
}
```

## 3. Bet (BET)

`POST` **`/wagering/transactions`** —
provider identity. The `Idempotency-Key` is required.

```sh
EXT_ID=ext-$(date +%s)

curl -s -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: bet-$EXT_ID" \
  -d "{
    \"providerId\": \"$PROVIDER_ID\",
    \"externalTransactionId\": \"$EXT_ID\",
    \"playerId\": \"$PLAYER_ID\",
    \"walletId\": \"$WALLET_ID\",
    \"roundId\": \"round-7\",
    \"gameId\": \"slot-fruit\",
    \"kind\": \"BET\",
    \"money\": { \"amount\": \"10.00\", \"currency\": \"BRL\" }
  }" | jq .
```

Response (✅ `200`) with the balance already
debited:

```json
{
  "transactionId": "a77b1c2d-3e4f-5061-7283-94a5b6c7d8e9",
  "status": "PROCESSED",
  "balance": { "amount": "990.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

## 4. Query the balance

`GET` **`/wallets/{walletId}`** —
internal identity.

```sh
curl -s "$API/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

## 5. Read the ledger

`GET` **`/wallets/{walletId}/ledger`**

```sh
curl -s "$API/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

For the next page, use the returned opaque cursor:

```sh
CURSOR=$(curl -s "$API/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $TOKEN_INT" | jq -r .nextCursor)

curl -s "$API/wallets/$WALLET_ID/ledger?limit=50&cursor=$CURSOR" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

> [!NOTE]
> **Page limit**
>
> The default is `50`; the maximum is
> `WALLET_LEDGER_PAGE_MAX=200`. Out-of-range values are clamped,
> not refused.


## 6. Idempotent replay

Resend **the same** request with **the same** key. Money does not move again;
the response carries the original outcome.

```sh
curl -s -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: bet-$EXT_ID" \
  -d "{
    \"providerId\": \"$PROVIDER_ID\",
    \"externalTransactionId\": \"$EXT_ID\",
    \"playerId\": \"$PLAYER_ID\",
    \"walletId\": \"$WALLET_ID\",
    \"roundId\": \"round-7\",
    \"gameId\": \"slot-fruit\",
    \"kind\": \"BET\",
    \"money\": { \"amount\": \"10.00\", \"currency\": \"BRL\" }
  }" | jq .
```

```json
{
  "transactionId": "a77b1c2d-3e4f-5061-7283-94a5b6c7d8e9",
  "status": "PROCESSED",
  "balance": { "amount": "990.00", "currency": "BRL" },
  "idempotentReplay": true
}
```

> [!TIP]
> **Compare the balances**
>
> Query the balance (step 4) after the replay: it is still
> `990.00`. The debit happened exactly once.


## 7. Trigger a 409 IDEMPOTENCY_KEY_CONFLICT

Reuse the **same key** with a **different payload** (a different amount). The
key already exists with another `payload_hash`.

```sh
curl -s -o /dev/null -w "%{http_code}\n" -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: bet-$EXT_ID" \
  -d "{
    \"providerId\": \"$PROVIDER_ID\",
    \"externalTransactionId\": \"$EXT_ID\",
    \"playerId\": \"$PLAYER_ID\",
    \"walletId\": \"$WALLET_ID\",
    \"roundId\": \"round-7\",
    \"gameId\": \"slot-fruit\",
    \"kind\": \"BET\",
    \"money\": { \"amount\": \"999.00\", \"currency\": \"BRL\" }
  }"
```

```json
{
  "error": {
    "code": "IDEMPOTENCY_KEY_CONFLICT",
    "message": "idempotency key reused with a different payload"
  }
}
```

## 8. Trigger a 422 INSUFFICIENT_FUNDS

Bet more than the balance. The rejection is **persisted** (status `REJECTED`)
and the body carries the domain code.

```sh
EXT_BIG=ext-big-$(date +%s)

curl -s -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: bet-$EXT_BIG" \
  -d "{
    \"providerId\": \"$PROVIDER_ID\",
    \"externalTransactionId\": \"$EXT_BIG\",
    \"playerId\": \"$PLAYER_ID\",
    \"walletId\": \"$WALLET_ID\",
    \"roundId\": \"round-8\",
    \"gameId\": \"slot-fruit\",
    \"kind\": \"BET\",
    \"money\": { \"amount\": \"999999.00\", \"currency\": \"BRL\" }
  }" | jq .
```

```json
{
  "transactionId": "b88c2d3e-4f50-6172-8394-a5b6c7d8e9f0",
  "status": "REJECTED",
  "balance": { "amount": "990.00", "currency": "BRL" },
  "idempotentReplay": false,
  "failureCode": "INSUFFICIENT_FUNDS",
  "failureMessage": "insufficient funds"
}
```

The HTTP status is ❌ `422`.

> [!WARNING]
> **Resending returns the same 422**
>
> Because the transaction was persisted as `REJECTED`, a replay
> with the same key returns the same body — with
> `idempotentReplay: true`. It is not a new attempt.


## 9. REFUND with a missing reference → 202 PENDING_REFERENCE

A `REFUND` requires `referenceExternalTransactionId`. If the reference is not
yet resolvable, the transaction is born `PENDING_REFERENCE` and the response is
ℹ️ `202`.

```sh
EXT_REF=ext-refund-$(date +%s)

curl -s -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: refund-$EXT_REF" \
  -d "{
    \"providerId\": \"$PROVIDER_ID\",
    \"externalTransactionId\": \"$EXT_REF\",
    \"playerId\": \"$PLAYER_ID\",
    \"walletId\": \"$WALLET_ID\",
    \"roundId\": \"round-7\",
    \"gameId\": \"slot-fruit\",
    \"kind\": \"REFUND\",
    \"money\": { \"amount\": \"10.00\", \"currency\": \"BRL\" },
    \"referenceExternalTransactionId\": \"does-not-exist-000\"
  }" | jq .
```

```json
{
  "transactionId": "c99d3e4f-5061-7283-94a5-b6c7d8e9f0a1",
  "status": "PENDING_REFERENCE",
  "balance": { "amount": "990.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

> [!NOTE]
> **What resolves the pending state**
>
> A reference worker watches the outbox and, once the referenced transaction
> appears, promotes the pending one to `PROCESSED`. Poll by id
> until the state changes (step 11).


## 10. Reconcile

`POST` **`/wallets/{walletId}/reconciliation`** —
internal identity. It changes nothing; it only compares.

```sh
curl -s -X POST "$API/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

```json
{
  "walletId": "8a2f5c1e-77b9-4a11-9d02-0e1f2a3b4c5d",
  "storedBalance": { "amount": "990.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "990.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 1
}
```

## 11. Read a transaction

`GET` **`/wagering/transactions/{transactionId}`**

```sh
TX_ID=$(jq -r .transactionId /tmp/wallet.json 2>/dev/null || echo "")

curl -s "$API/wagering/transactions/$TX_ID" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

Or by the provider identity:

```sh
curl -s "$API/providers/$PROVIDER_ID/wagering/transactions/$EXT_ID" \
  -H "Authorization: Bearer $TOKEN" | jq .
```

## Scenario roll-up

| Scenario | Key request | Expected HTTP |
| :-- | :-- | :-- |
| Valid bet | new payload, new key | ✅ `200` `PROCESSED` |
| Replay | same key, same payload | ✅ `200` `idempotentReplay: true` |
| Key conflict | same key, different payload | ❌ `409` `IDEMPOTENCY_KEY_CONFLICT` |
| No funds | amount > balance | ❌ `422` `INSUFFICIENT_FUNDS` |
| Pending refund | missing reference | ℹ️ `202` `PENDING_REFERENCE` |

See also: [API reference](reference.md) · [Errors](errors.md) ·
[Idempotency](idempotency.md).
