> Receitas `curl` completas e copiáveis, na ordem em que um provedor
> as executa: obter token, abrir carteira, apostar, consultar, reconciliar — e
> provocar de propósito o 409, o 422 e o 202 para ver cada caminho do contrato.

## Variáveis de ambiente

Defina uma vez; os blocos abaixo usam estas variáveis.

```sh
API=http://localhost:8080
KEYCLOAK=http://localhost:8080/realms/wallet/protocol/openid-connect/token

# Identidades disponíveis no realm de desenvolvimento (deploy/keycloak).
INTERNAL_ID=wallet-internal
INTERNAL_SECRET=internal-secret
PROVIDER_ID=provider-a
PROVIDER_SECRET=provider-a-secret
```

> [!TIP]
> **Confira o binário e o compose**
>
> `make up` sobe a stack completa. As rotas de negócio só existem fora do
> modo worker; em `WALLET_MODE=worker` elas retornam
> ❌ `404` de propósito.


## 1. Obter token no Keycloak

O serviço valida o JWT por JWKS e exige o audience `wallet-api`. Cada client
já nasce com o scope certo (`wallet:internal` ou `wallet:provider`).

```sh
TOKEN=$(curl -s -X POST "$KEYCLOAK" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=$PROVIDER_ID" \
  -d "client_secret=$PROVIDER_SECRET" \
  | jq -r .access_token)

echo "$TOKEN"
```

Para as rotas internas, repita com a identidade interna:

```sh
TOKEN_INT=$(curl -s -X POST "$KEYCLOAK" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=$INTERNAL_ID" \
  -d "client_secret=$INTERNAL_SECRET" \
  | jq -r .access_token)
```

> [!WARNING]
> **Sem token, sem conversation**
>
> Qualquer rota de negócio sem `Authorization` válido devolve
> ❌ `401`
> `UNAUTHORIZED`. Um token de outro audience é recusado da mesma
> forma.


## 2. Abrir uma carteira

`POST` **`/wallets`** — identidade interna.

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

Resposta (✅ `201`):

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

## 3. Apostar (BET)

`POST` **`/wagering/transactions`** —
identidade de provedor. O `Idempotency-Key` é obrigatório.

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

Resposta (✅ `200`) com o saldo já debitado:

```json
{
  "transactionId": "a77b1c2d-3e4f-5061-7283-94a5b6c7d8e9",
  "status": "PROCESSED",
  "balance": { "amount": "990.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

## 4. Consultar saldo

`GET` **`/wallets/{walletId}`** —
identidade interna.

```sh
curl -s "$API/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

## 5. Ler o ledger

`GET` **`/wallets/{walletId}/ledger`**

```sh
curl -s "$API/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

Para a próxima página, use o cursor opaco devolvido:

```sh
CURSOR=$(curl -s "$API/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $TOKEN_INT" | jq -r .nextCursor)

curl -s "$API/wallets/$WALLET_ID/ledger?limit=50&cursor=$CURSOR" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

> [!NOTE]
> **Limite de página**
>
> O padrão é `50`; o máximo é
> `WALLET_LEDGER_PAGE_MAX=200`. Valores fora da faixa são
> clampeados, não recusados.


## 6. Replay idempotente

Reenvie **a mesma** requisição, com **a mesma** chave. O dinheiro não move de
novo; a resposta traz o resultado original.

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
> **Compare os saldos**
>
> Consulte o saldo (passo 4) depois do replay: ele continua
> `990.00`. O débito aconteceu uma única vez.


## 7. Provocar 409 IDEMPOTENCY_KEY_CONFLICT

Reuse a **mesma chave** com um **payload diferente** (outro valor). A chave já
existe com outro `payload_hash`.

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

## 8. Provocar 422 INSUFFICIENT_FUNDS

Aposte mais do que o saldo. A rejeição é **persistida** (status `REJECTED`) e o
corpo carrega o código de domínio.

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

O status HTTP é ❌ `422`.

> [!WARNING]
> **Reenviar devolve o mesmo 422**
>
> Como a transação foi persistida como `REJECTED`, um replay com a
> mesma chave devolve o mesmo corpo — com
> `idempotentReplay: true`. Não é uma nova tentativa.


## 9. REFUND com referência inexistente → 202 PENDING_REFERENCE

Um `REFUND` exige `referenceExternalTransactionId`. Se a referência ainda não
for resolvível, a transação nasce `PENDING_REFERENCE` e a resposta é
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
    \"referenceExternalTransactionId\": \"nao-existe-000\"
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
> **O que resolve a pendência**
>
> Um worker de referência observa o outbox e, quando a transação referenciada
> aparece, promove a pendente para `PROCESSED`. Consulte pelo id
> até o estado mudar (passo 11).


## 10. Reconciliar

`POST` **`/wallets/{walletId}/reconciliation`** —
identidade interna. Não altera nada; só compara.

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

## 11. Ler uma transação

`GET` **`/wagering/transactions/{transactionId}`**

```sh
TX_ID=$(jq -r .transactionId /tmp/wallet.json 2>/dev/null || echo "")

curl -s "$API/wagering/transactions/$TX_ID" \
  -H "Authorization: Bearer $TOKEN_INT" | jq .
```

Ou pela identidade do provedor:

```sh
curl -s "$API/providers/$PROVIDER_ID/wagering/transactions/$EXT_ID" \
  -H "Authorization: Bearer $TOKEN" | jq .
```

## Rol de cenários

| Cenário | Requisição-chave | HTTP esperado |
| :-- | :-- | :-- |
| Aposta válida | payload novo, chave nova | ✅ `200` `PROCESSED` |
| Replay | mesma chave, mesmo payload | ✅ `200` `idempotentReplay: true` |
| Conflito de chave | mesma chave, payload diferente | ❌ `409` `IDEMPOTENCY_KEY_CONFLICT` |
| Sem saldo | valor > saldo | ❌ `422` `INSUFFICIENT_FUNDS` |
| Refund pendente | referência inexistente | ℹ️ `202` `PENDING_REFERENCE` |

Veja também: [Referência da API](reference.md) · [Erros](errors.md) ·
[Idempotência](idempotency.md).
