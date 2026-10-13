# SQS as code (`deploy/tofu`)

A declaração declarativa das três filas SQS do serviço. Ela **não substitui**
`deploy/localstack/init/10-queues.py` — as duas coexistem, e um teste impede que
divirjam.

---

## Por que existem duas

| | `10-queues.py` (boto3) | `deploy/tofu` |
| :-- | :-- | :-- |
| Roda em | `docker compose up`, dentro do container | a máquina de quem quiser, ou o CI |
| Precisa de toolchain? | **não** | sim (`tofu`) |
| Aplica resource policy / IAM / KMS? | não dá — o LocalStack ignora | **sim, contra AWS** |
| Detecta drift? | não (roda uma vez no boot) | sim (`tofu plan`) |
| É a fonte da verdade? | do ambiente local | da forma de produção |

O script boto é o que faz `make up` ser auto-suficiente: a imagem do LocalStack
monta o diretório em `/etc/localstack/init/ready.d` e o executa no boot. Essa
propriedade vale a pena manter. O que ele **não pode** fazer é a parte que não
existe localmente — medido em `deploy/localstack/README.md`: o LocalStack 4.14
community **armazena e não aplica** resource policy, e `iam`/`sts`/`kms` estão
`disabled`. Uma policy escrita para o LocalStack seria teatro.

O tofu é o lugar onde essa policy tem um alvo real.

---

## Uso

```sh
make tofu-plan      # lê contra o LocalStack, não provisiona nada
make tofu-apply     # aplica contra o LocalStack
make tofu-fmt       # formatação, como gofmt é para o Go
```

Contra AWS real: `-var sqs_endpoint=` (vazio) e remova as credenciais estáticas
de `provider.tf`, deixando a cadeia do SDK resolver. **Não** aponte as
credenciais `test`/`test` para uma conta real.

Como o script boto carrega o **mesmo conteúdo** das filas, `tofu apply` sobre um
LocalStack que já subiu pelo compose vai encontrar as filas existentes. É a
armadilha abaixo.

---

## O problema do state (leia antes de aplicar sem prefixo)

O tofu casa recursos por **identidade no state**, não por nome. As filas criadas
pelo `10-queues.py` existem no LocalStack mas **não** no state do tofu. Sem
cuidado, `tofu apply` tenta **criá-las** e bate no mesmo `QueueAlreadyExists` que
o script boto já teve que contornar — a diferença é que no tofu a saída é
manual:

```sh
tofu import aws_sqs_queue.work    http://localhost:4566/000000000000/wager-transactions.fifo
tofu import aws_sqs_queue.dlq     http://localhost:4566/000000000000/wager-transactions-dlq.fifo
tofu import aws_sqs_queue.events  http://localhost:4566/000000000000/integration-events.fifo
```

Por isso o uso normal com um LocalStack já povoado é com **prefixo**, que cria um
conjunto paralelo e não colide:

```sh
make tofu-apply PREFIX=probe-
make tofu-destroy PREFIX=probe-
```

É assim que o módulo foi verificado: as três filas com prefixo, comparadas
atributo por atributo contra as três do script boto, e removidas no fim.

---

## O que a medição revelou

Escrever isto contra uma stack viva encontrou três coisas que a leitura do
provider não mostraria.

**1. `max_message_size` e o range do provider.** O default do provider
`hashicorp/aws` v5 é **262144 (256 KiB)**, não o 1 MiB do SQS. Pior: o v5
**rejeita** 1 MiB —

```
aws v5.100.0: Error: expected max_message_size to be in the range (1024 - 262144), got 1048576
aws v6.68.0: aplica; a fila volta MaximumMessageSize=1048576
```

Isso não é cosmético. `internal/transport/http/codecs.go` lê corpos de até
`1<<20` (1 MiB), então uma requisição que a API **aceita** seria **recusada** na
publicação se a fila tivesse 256 KiB. Por isso o módulo exige `~> 6.0` e pina o
valor em `max_message_size_bytes`.

**2. O mesmo valor estava implícito no script boto.** O `10-queues.py` não
declarava `MaximumMessageSize` em lugar nenhum — herdava o default do servidor.
Uma das três filas, num volume antigo do LocalStack, estava com **256 KiB**
enquanto as outras duas tinham 1 MiB. Não é reproduzível do zero (recriar com os
mesmos atributos dá 1 MiB), o que torna o caso pior, não melhor: era um valor
que ninguém escolheu, variando por idade de volume. O script passou a pinar
`MAX_MESSAGE_SIZE` explicitamente.

**3. `integration-events.fifo` não declarava `VisibilityTimeout`.** Ficava com o
default de 30s do SQS enquanto as outras duas declaravam 60. O teste de sincronia
pegou a divergência entre os dois arquivos. Corrigido para 60, explícito.

Nenhuma das três apareceria sem rodar contra um servidor.

---

## Sincronia com o script boto

`test/integration/tofu_sqs_sync_test.go` (sem build tag, roda no `go test ./...`
rápido) lê **os dois** arquivos e compara, por fila:

- `VisibilityTimeout`, `ReceiveMessageWaitTimeSeconds`, `MessageRetentionPeriod`
  e `MaximumMessageSize` — o valor no `.py` contra o default da variável no
  `.tf`, mais a checagem de que o recurso tofu está de fato **ligado à variável**
  (`var.<nome>`) em vez de um literal inline;
- `maxReceiveCount` e a presença da `redrive_policy`;
- `content_based_deduplication = false` na fila de events, cujo dedup é por
  **identidade do evento**, nunca por hash do payload;
- os três nomes de fila nos dois arquivos.

Duas declarações de uma verdade divergem — o repositório já pagou por essa lição
entre o compose e o chart. O custo de prevenir aqui é um parse.

---

## Limite honesto

Apontar isto para AWS real é uma **declaração, não uma medição**: não havia
credenciais AWS disponíveis. O que foi provado, contra o LocalStack vivo:

- a configuração valida e formata;
- 7 recursos (3 filas, 1 redrive, 3 policies) aplicam e são destruídos;
- os atributos *aplicados pelo servidor* das três filas do tofu são **idênticos**
  aos das três do script boto — lidos com `get-queue-attributes`, não do state.

O que **não** foi provado: que a resource policy realmente nega algo em produção.
No LocalStack ela é aceita e ignorada; é exatamente o que
`TestSQSResourcePolicyIsAcceptedButNotEnforced` fixa em teste, e que vai começar
a falhar no dia em que um LocalStack a aplicar.
