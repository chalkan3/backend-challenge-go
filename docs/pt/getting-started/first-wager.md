> Sua primeira aposta, do zero: token de *provedor*, um `BET`
> com `Idempotency-Key`, a leitura da resposta e — o ponto que importa
> — a prova de que repetir a mesma chave devolve o mesmo resultado sem mover
> dinheiro duas vezes.

## Pré-requisitos

Esta página assume a stack de pé (`make up`) e uma carteira aberta. Se ainda não
fez isso, comece pelo [Início rápido](quickstart.md).

```sh
API=http://localhost:8081
PLAYER=0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1

# token interno: abre e lê carteiras
TOKEN_INT=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=wallet-internal -d client_secret=internal-secret | jq -r .access_token)

# token de provedor: submete apostas
TOKEN_A=$(curl -s http://localhost:8080/realms/wallet/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-secret | jq -r .access_token)

# abre a carteira (saldo inicial) e guarda o id
WALLET=$(curl -s -X POST $API/wallets \
  -H "Authorization: Bearer $TOKEN_INT" -H 'Content-Type: application/json' \
  -d '{"playerId":"'$PLAYER'","initialBalance":{"amount":"1000.00","currency":"BRL"}}' \
  | jq -r .id)
echo "wallet = $WALLET"
```

> [!NOTE]
> **Por que dois tokens**
>
> `wallet-internal` (escopo `wallet:internal`) abre e lê carteiras. Já a
> **aposta** exige um token de **provedor** (`provider-a`, escopo
> `wallet:provider`): o serviço deriva o `providerId` das claims assinadas e
> recusa operar em nome de outro provedor.


## O fluxo de uma aposta

```mermaid
sequenceDiagram
    autonumber
    participant P as Provedor (você)
    participant KC as Keycloak
    participant API as HTTP (go-kit)
    participant DB as PostgreSQL

    P->>KC: POST /token (client_credentials, provider-a)
    KC-->>P: access_token (wallet:provider)
    P->>API: POST /wagering/transactions  
Bearer + Idempotency-Key
    API->>API: valida JWT (JWKS) e autoriza provider-a
    API->>DB: BEGIN · FOR UPDATE · INSERT · UPDATE · COMMIT
    DB-->>API: saldo atualizado
    API-->>P: 200 { transactionId, status: PROCESSED, balance }

    Note over P,API: REPLAY — mesma chave, mesmo corpo
    P->>API: POST /wagering/transactions  
mesma Idempotency-Key
    API-->>P: 200 idêntico · idempotentReplay: true  
(sem novo lançamento)
```

## O primeiro BET

Uma aposta é um `POST /wagering/transactions` com `Idempotency-Key`
**obrigatório**:

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

Resposta:

```json
{
  "transactionId": "0192f28f-7b12-7a44-bd10-3f9c1e2a4b55",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

| Campo | O que significa |
| :-- | :-- |
| `transactionId` | id **interno** da operação (não é o `externalTransactionId`) |
| `status` | `PROCESSED` quando o dinheiro já se moveu |
| `balance` | saldo da carteira **depois** da operação |
| `idempotentReplay` | `false` na primeira execução, `true` num replay |

O `money` é sempre **string decimal + moeda** — nunca `float`. O serviço guarda
em unidades menores (`int64`) e devolve a mesma representação.

> [!NOTE]
> **Os kinds**
>
> `BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`. O `LOSS` é **informacional**
> (`amount = 0`): registra que a rodada não pagou prêmio, sem mover saldo.
> `REFUND`/`ROLLBACK` exigem `referenceExternalTransactionId` para uma
> operação do **mesmo** `roundId`, jogador, carteira e moeda.


## O REPLAY: repetir move dinheiro uma vez só

Repita **exatamente** a mesma requisição — mesma chave, mesmo corpo:

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

A resposta é a **mesma**, exceto por um campo:

```json
{
  "transactionId": "0192f28f-7b12-7a44-bd10-3f9c1e2a4b55",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": true
}
```

Mesmo `transactionId`, mesmo saldo (`975.00`, **não** `950.00`),
`"idempotentReplay": true`. O saldo não se moveu porque o lançamento no ledger
não foi repetido.

> [!TIP]
> **Prove você mesmo**
>
> Confira o saldo depois do replay — ele continua `975.00`:
> ```sh
> curl -s $API/wallets/$WALLET -H "Authorization: Bearer $TOKEN_INT" | jq .balance
> ```


## O conflito: mesma chave, corpo diferente

A identidade da idempotência é a chave **mais** o conteúdo. Reutilizar a chave
com um corpo diferente é um erro, e o serviço responde `409`:

```sh
# mesma Idempotency-Key, mas amount diferente
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

| Cenário | Status | Código |
| :-- | :-- | :-- |
| Mesma chave, mesmo corpo | `200` | — (`idempotentReplay: true`) |
| Mesma chave, corpo diferente | `409` | `IDEMPOTENCY_KEY_CONFLICT` |
| `externalTransactionId` já usado de outra forma | `409` | `EXTERNAL_TRANSACTION_ID_CONFLICT` |
| Saldo insuficiente | `422` | `INSUFFICIENT_FUNDS` |
| Referência divergente | `422` | `REFERENCE_MISMATCH` |

> [!WARNING]
> **A resposta idêntica é a garantia, não um detalhe**
>
> Um retry de rede que "por acaso" atingiu duas réplicas não pode debitar
> duas vezes. O resultado da primeira execução é **persistido** e replicado
> no replay: por isso a resposta é byte a byte a mesma, e apenas o flag
> `idempotentReplay` muda.


## Ler o ledger

O saldo é um *cache derivado*; a verdade está no ledger **append-only**. Cada
movimento é uma linha imutável:

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

Há **dois** lançamentos para **uma** aposta — a abertura não duplicou (o replay
não inseriu nada) e a BET debitou `25.00`.

Confirme que o saldo é a soma dos lançamentos:

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

`"consistent": true` e `difference` igual a zero significam que o saldo bate com
o ledger.

Veja também: [Início rápido](quickstart.md) · [5 minutos](five-minutes.md) ·
[Idempotência](../api/idempotency.md) · [Ledger](../data/ledger.md).
