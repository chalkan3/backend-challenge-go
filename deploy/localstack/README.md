# LocalStack — provisioning e políticas das filas SQS

Este diretório provisiona o broker SQS local. O provisioning é **imperativo e
idempotente**: `init/10-queues.py` roda no `ready.d` do LocalStack, cria as
filas se faltarem e **reconcilia** os atributos se já existirem.

Ele vive aqui, e não num diretório de "políticas", porque a distinção que
importa não é o nome do arquivo — é se o servidor **aplica** o que está escrito.
Este documento registra o que foi medido, inclusive o que **não** funciona.

---

## As filas e suas políticas

| Fila | Papel |
| :-- | :-- |
| `wager-transactions.fifo` | Fila de trabalho que o consumer drena |
| `wager-transactions-dlq.fifo` | Quarentena de mensagens envenenadas |
| `integration-events.fifo` | Destino do relay de outbox |

### `wager-transactions.fifo`

| Atributo | Valor | Por quê |
| :-- | :-- | :-- |
| `RedrivePolicy.maxReceiveCount` | `5` | Uma mensagem envenenada para de ser retentada e vai para a DLQ — a diferença entre "o consumer está ocupado" e "o consumer nunca vai conseguir" |
| `VisibilityTimeout` | `60` | **Precisa** exceder o orçamento de processamento do consumer, ou uma mensagem correta mas lenta é reentregue *em voo* — o bug clássico de processamento duplicado |
| `ReceiveMessageWaitTimeSeconds` | `20` | Long polling: o consumer bloqueia no socket em vez de girar com respostas vazias. 20 é o máximo da AWS |
| `MessageRetentionPeriod` | `345600` (4 dias) | Uma queda maior que isso perde a requisição; explícito para que o limite seja decisão, não default |
| `ContentBasedDeduplication` | `true` | O publisher manda `MessageDeduplicationId` explícito; dedup por identidade |
| `SqsManagedSseEnabled` | `true` | Criptografia em repouso com chave gerenciada |

### `wager-transactions-dlq.fifo`

| Atributo | Valor | Por quê |
| :-- | :-- | :-- |
| `MessageRetentionPeriod` | `1209600` (14 dias, o máximo) | Mensagem na DLQ é **evidência de um bug**. 4 dias não é tempo de perceber. Retenção diferente da fila de origem, de propósito |

### `integration-events.fifo`

| Atributo | Valor | Por quê |
| :-- | :-- | :-- |
| `ContentBasedDeduplication` | `false` | O relay fornece `MessageDeduplicationId` = **id do evento**, então a dedup é pela identidade do evento, nunca por hash do corpo. Dois eventos distintos com corpos iguais não podem colapsar |
| `MessageRetentionPeriod` | `1209600` (14 dias) | São eventos financeiros |

---

## O limite medido: resource policy NÃO é aplicada no LocalStack

Antes de escrever qualquer provisionamento, isto foi medido no LocalStack
**4.14.0, community**:

```
# Escreve um Deny-all na fila...
sqs set-queue-attributes --attributes '{"Policy":"{...Deny * on sqs:*...}"}'

# ...e o SendMessage PASSA mesmo assim.
sqs send-message ... → MessageId: 866a16d9-...
```

A `Policy` é **armazenada mas não aplicada**. O mesmo vale para IAM/STS: no
`_localstack/health`, `iam` e `sts` estão `disabled` — só `sqs` está `running`.

**Consequência prática, e a razão deste documento existir:** provisionar uma
resource policy aqui seria **teatro** — config que parece proteção e não
protege. Então **nenhuma resource policy é provisionada**. O teste
`TestSQSResourcePolicyIsAcceptedButNotEnforced` fixa esse comportamento: ele
prova que a policy é aceita e ignorada, e vai **começar a falhar** se um
LocalStack futuro passar a aplicá-las — que é exatamente quando o
provisionamento deve ser revisitado para adicionar uma.

Em **AWS real**, o que faria sentido e não existe aqui:

- Resource policy na fila: `Deny` de `sqs:SendMessage` para principals que não
  sejam o publisher; `Deny` de qualquer ação sem `aws:SecureTransport`.
- IAM role por serviço (consumer com `ReceiveMessage`/`DeleteMessage`;
  publisher só com `SendMessage`), em vez das credenciais estáticas `test`/`test`.
- KMS com chave gerenciada pelo cliente em vez da chave gerenciada pela AWS.

---

## O bug que a medição revelou: `create_queue` não é idempotente

`create_queue` é rejeitado com `QueueAlreadyExists` quando os atributos diferem
dos da fila existente — **e os atributos antigos permanecem**. Um script que só
chama `create_queue` é um bootstrap de uma vez só, não provisioning: editar um
atributo e re-rodar o script falha silenciosamente para quem não lê o erro.

`ensure_queue` resolve isso: tenta `create_queue`, captura `QueueNameExists` e
cai para `get_queue_url`, depois **sempre** aplica `set_queue_attributes`. O
script passa a ser seguro de re-rodar e *reconcilia* uma fila existente.

---

## Valores que estavam implícitos, e agora são decisões

Escrever o módulo [`../tofu`](../tofu/) ao lado deste script expôs três valores
que ninguém tinha escolhido — os dois primeiros neste arquivo.

**`MaximumMessageSize` não era declarado.** O script herdava o default do
servidor. Uma das três filas, num volume antigo do LocalStack, estava com **256
KiB** enquanto as outras duas tinham 1 MiB. Recriar do zero com os mesmos
atributos dá 1 MiB, ou seja, o valor dependia da idade do volume — o que torna o
caso pior, não melhor. O script agora declara `MAX_MESSAGE_SIZE = "1048576"`.
Importa porque `internal/transport/http/codecs.go` lê corpos de até `1<<20`:
uma fila de 256 KiB recusaria uma mensagem que a API já validou.

**`VisibilityTimeout` faltava em `integration-events.fifo`.** Rodava com o
default de 30s do SQS enquanto as outras duas declaravam 60. Passou a declarar 60
como as demais.

**No provider Terraform/OpenTofu**, o default de `max_message_size` é 262144 e o
`hashicorp/aws` v5 **rejeita** 1 MiB — o v6 aceita. É por isso que o módulo exige
`~> 6.0`; ver [`../tofu/README.md`](../tofu/README.md).

---

## Como isto é provado

`test/integration/sqs_policy_test.go`, contra o broker **vivo**:

| Teste | O que prova |
| :-- | :-- |
| `TestSQSPoliciesAreApplied` | Lê cada atributo de volta e confere `RedrivePolicy`, `VisibilityTimeout`, retenção, long polling e dedup |
| `TestSQSRedrivePolicyIsEnforced` | **Comportamental**: publica, recebe além de `maxReceiveCount` e observa a mensagem **chegar na DLQ** — prova que a política *age*, não só que foi gravada |
| `TestSQSResourcePolicyIsAcceptedButNotEnforced` | Documenta o limite do LocalStack e falhará quando ele mudar |
| `TestTofuSQSMatchesLocalstackInit` | Compara este script com `deploy/tofu` por fila: os valores e a ligação às variáveis |

Rodar:

```sh
WALLET_TEST_SQS_ENDPOINT=http://localhost:4566 \
  go test -tags=integration ./test/integration/ -run TestSQS -v
```

---

## Reaplicar o provisioning sem reiniciar o container

O `ready.d` só roda no boot do LocalStack. Para reconciliar as filas já no ar:

```sh
podman exec wallet_localstack_1 python3 /etc/localstack/init/ready.d/10-queues.py
```

É idempotente: rodar duas vezes não muda nada na segunda.
