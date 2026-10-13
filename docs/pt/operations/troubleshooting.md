> Os problemas abaixo são os que **realmente aconteceram** neste
> projeto. Cada um traz o sintoma exato, a causa medida e o comando que resolve.
> Onde um comportamento parece erro mas é contrato, isso está dito
> explicitamente.

> [!TIP]
> **Antes de acusar o código**
>
> Um `make preflight` custa dois segundos e elimina metade desta página.
> A outra metade costuma ser o LocalStack, com limites medidos e documentados.


## Ciclo de vida do ambiente

### Porta ocupada

**Sintoma:** a subida falha imediatamente com a porta já em uso.

**Causa:** outra stack (um cluster `kind`, uma execução anterior) está segurando
a porta.

**Solução:** rode o preflight, que checa as portas antes de subir.

```sh
make preflight        # scripts/port-check.sh
```

| Porta | Quem usa |
| :-- | :-- |
| `5433` | Postgres. |
| `8080` | API local / Keycloak. |
| `4566` | LocalStack (SQS). |
| `8081` | API nos containers / multi-réplica. |

### `make e2e` derruba a stack no teardown

**Sintoma:** depois de um `make e2e` bem-sucedido, nada mais responde.

**Causa:** o e2e é **hermético de propósito** — ele sobe a stack, roda e a
derruba, inclusive quando passa.

**Solução:** re-subir com `make up`, ou pedir para manter.

```sh
make up                  # reergue o ambiente
make e2e KEEP=1          # roda o e2e e deixa a stack no ar
UP_ONLY=1 ./scripts/e2e.sh   # sobe a stack e para (útil para o k6 dirigir)
```

### `podman-compose down` falha com profiles ativos

**Sintoma:** o `down` reclama de serviços que pertencem a um profile.

**Causa:** conhecida do `podman-compose` com serviços de profile.

**Solução:** remover os containers à força.

```sh
podman rm -f 
```

### `up` não recria por tag de imagem igual

**Sintoma:** você mudou o código, `make up` roda, mas o comportamento é o antigo.

**Causa:** a tag da imagem é a mesma, então o engine não recria o container.

**Solução:** forçar a recriação.

```sh
docker compose up -d --force-recreate
```

## Serviços com armadilhas

### Keycloak `--import-realm` usa `IGNORE_EXISTING`

**Sintoma:** o realm no Keycloak não reflete o `realm-wallet.json`.

**Causa:** `--import-realm` usa a estratégia **`IGNORE_EXISTING`**: um realm
*existente* **não** é reimportado. Editar o JSON e reiniciar **não** aplica a
mudança.

**Detecção:** `realm_sync_test.go` falha **alto** contra um realm obsoleto —
este é o sintoma esperado, não um falso positivo.

**Solução:** remover o realm/volume antes de reimportar.

> [!WARNING]
> **Não é um bug do Keycloak — é a semântica do `IGNORE_EXISTING`**
>
> O teste existe exatamente porque essa armadilha é silenciosa de outro modo:
> sem ele, o realm obsoleto passaria batido.


### LocalStack: limites medidos

| Sintoma | Causa medida | Saída |
| :-- | :-- | :-- |
| `Deny * on sqs:*` não bloqueia nada | A resource policy é **armazenada mas não aplicada** no community | Escreva a política; saiba que ela só vale em AWS real |
| `create_queue` "não aplica" a mudança de atributo | `create_queue` **não é idempotente** para mudança de atributo | `get_queue_url` + `set_queue_attributes` (o que o script faz) |
| IAM/STS/KMS indisponíveis | Estão `disabled` em `_localstack/health` | Não dependa de CMK nem de separação por IAM localmente |

> [!NOTE]
> **O tofu não é a saída para o LocalStack**
>
> As políticas no `deploy/tofu` descrevem a forma que elas **deveriam** ter em
> AWS. Ver [Infraestrutura como código](infrastructure.md): é uma declaração,
> não uma prova local.


## Respostas que parecem erro e são contrato

Estas são cometidas por comportamento **correto**. Antes de abrir um bug,
confira a coluna da leitura.

| Resposta | Leitura | Ação |
| :-- | :-- | :-- |
| `503` em `/health/ready` | Postgres ou SQS inalcançável — é **o contrato do endpoint** | Consertar a dependência, não o endpoint |
| `409 IDEMPOTENCY_KEY_CONFLICT` | A **mesma chave** chegou com **corpo diferente** | Corrigir o cliente, que reusou a chave |
| `202 PENDING_REFERENCE` | `REFUND`/`ROLLBACK` referenciando uma transação que **ainda não existe** | **Não é erro**: o reference worker resolve |
| `422 INSUFFICIENT_FUNDS` | Rejeição de negócio **persistida** | Nada a consertar; a rejeição está gravada |

> [!NOTE]
> **`202 PENDING_REFERENCE` é o caso mais confundido com falha**
>
> Um reembolso pode chegar **antes** da transação que ele reverte — a fila
> FIFO garante ordem por grupo, mas os dois eventos podem ser gerados em
> momentos distintos. O serviço aceita, marca como pendente e resolve depois.
> Responder `2xx` ali é o comportamento certo.


> [!WARNING]
> **`422` também é persistido**
>
> A rejeição por fundos insuficientes **não** é uma validação volátil: ela
> fica registrada. Se um teste espera "nada foi gravado", ele está errado.


### Diagnóstico rápido das respostas

```sh
# 503: o readiness está falando de uma dependência, não do processo.
curl -s localhost:8080/health/ready

# 409: confira se a chave reusada carregou o mesmo corpo.
curl -si -X POST localhost:8080/v1/wallets/$ID/transactions \
  -H "Idempotency-Key: $KEY" -d @body.json

# 202: a transação referenciada ainda não existe; espere o worker.
curl -s localhost:8080/v1/transactions/$REF
```

## Sinais de telemetria como diagnóstico

Quando o problema não é o ambiente, a resposta costuma estar nas métricas.

| Métrica | Leitura |
| :-- | :-- |
| `wallet_outbox_pending` subindo | O relay não está drenando: olhe `WALLET_OUTBOX_INTERVAL` e `WALLET_OUTBOX_BATCH`. |
| `wallet_outbox_lag_seconds` alto | Evento antigo preso — o consumidor a jusante pode estar falhando. |
| `wallet_consumer_retries_total` subindo | Mensagens sendo reprocessadas; verifique a visibilidade (`WALLET_SQS_VISIBILITY`). |
| `wallet_consumer_dlq_total` > 0 | Mensagem envenenada; o payload está na DLQ para inspeção. |
| `wallet_reconciliation_divergences_total` > 0 | **Bug de correção** — pare e investigue. |
| `wallet_auth_failures_total` por motivo | O issuer/JWKS ou o `aud` estão desalinhados. |

```sh
# Os números, sem dashboard.
curl -s localhost:8080/metrics | grep ^wallet_
```

Veja também: [Configuração](configuration.md) ·
[Observabilidade](observability.md) ·
[Infraestrutura](infrastructure.md) ·
[Testes](testing.md).
