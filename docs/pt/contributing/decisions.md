> Cada decisão não óbvia deste projeto tem uma razão que pode ser medida. Esta
> página é o registro de *por que*: o que foi escolhido, o que foi recusado e
> qual modo de falha a escolha evita. Não é um diário — é um contrato com quem vai
> mexer no código depois.

> [!TIP]
> **Como ler esta página**
>
> Cada seção segue a mesma forma: **Decisão**, **Alternativa recusada**,
> **Por quê**. Se você está prestes a reverter uma delas, leia o modo de falha
> primeiro.


## UUIDv7 em vez de UUIDv4 (ou sequencial)

**Decisão:** todos os IDs são UUIDv7.

**Recusado:** UUIDv4 (aleatório) e `bigserial` (gerador central).

**Por quê:** UUIDv7 é ordenado no tempo. Inserções vão para o **fim** do índice
B-tree — sem *page splits* constantes, o problema clássico do v4. E, ao contrário
de um `bigserial`, não exige coordenação central: três réplicas geram IDs sem se
falar.

| Propriedade | Resultado |
| :-- | :-- |
| Ordenado no tempo | Localidade de índice. |
| Gerado localmente | Sem gargalo de sequência. |
| Único globalmente | Sem colisão entre réplicas. |

## Outbox transacional em vez de publicação direta (e de 2PC)

**Decisão:** o evento é escrito na **mesma transação** da mudança de estado e
publicado depois, de forma assíncrona.

**Recusado:** publicar direto para o SQS dentro do caso de uso; e 2PC
(commit em duas fases) entre Postgres e SQS.

**Por quê:** publicar depois do commit deixa uma janela — o processo morre entre
o commit e o `SendMessage`, e o evento se perde. Publicar antes do commit permite
que o evento saia e a transação sofra rollback, e aí o mundo recebe um fato que
não aconteceu. 2PC precisa que ambos os participantes suportem o protocolo — o
SQS não suporta. O outbox elimina a janela: se a transação commitou, o evento
existe; se rollback, o evento não existe.

> [!NOTE]
> **O que o outbox custa**
>
> Latência (o evento sai no próximo tick, não na mesma respiração) e um
> publisher com *lease*. Em troca, o efeito e o evento são **atômicos**.


## Inbox durável em vez de dedup só por SQS

**Decisão:** uma tabela de inbox registra a chave processada **na mesma
transação** do efeito.

**Recusado:** confiar no SQS FIFO e no `MessageDeduplicationId`.

**Por quê:** o SQS garante *entrega at-least-once*; a janela de deduplicação do
FIFO é de **5 minutos**. Uma mensagem reentregue **depois** dessa janela — após
um visibility timeout longo, um retry, ou um reprocessamento manual — passaria
como nova. O inbox durável deduplica por **tempo de armazenamento**, que é o
único que cobre a janela real de um incidente.

> [!WARNING]
> **O inbox não substitui o FIFO**
>
> O SQS continua fazendo sua parte (ordem por grupo, redrive para a DLQ). O
> inbox é a **segunda** camada, porque a primeira tem um teto de tempo.


## Lock por linha + versão otimista, não serializable global

**Decisão:** a carteira é travada (`SELECT ... FOR UPDATE`) e a linha carrega uma
`version`; a atualização é otimista sobre essa versão.

**Recusado:** isolamento `SERIALIZABLE` global.

**Por quê:** `SERIALIZABLE` global faz **toda** transação conflitar com as demais,
mesmo as que não se cruzam. Com apostas em carteiras diferentes — que é o caso
comum — o custo é retries em cascata sem nenhum ganho de correção. O lock por
linha serializa apenas o que realmente compete: **duas apostas na mesma
carteira**. As demais seguem em paralelo.

```sql
-- O conflito é localizado na linha da carteira, não no banco inteiro.
SELECT balance, version FROM wallets WHERE id = $1 FOR UPDATE;
UPDATE wallets SET balance = $2, version = version + 1 WHERE id = $1 AND version = $3;
```

> [!NOTE]
> **É por isso que o `hot-wallet` existe no teste de carga**
>
> O cenário `hot-wallet` aponta N VUs para **uma** carteira. Ele é a prova
> empírica de que o lock serializa sem perder atualização — e de que o resto do
> tráfego continua paralelo.


## Money em minor units `int64`, nunca `float`

**Decisão:** valores monetários são inteiros em unidades menores, mais a moeda.

**Recusado:** `float64`, `decimal` de biblioteca, aritmética de string.

**Por quê:** `float64` não representa `0.1` exatamente; somas de saldo acumulam
erro e um ledger que não fecha não tem conserto depois. Unidades menores em
`int64` são exatas por construção, e a moeda viaja junto para que somar moedas
diferentes seja um **erro**, não uma conversão silenciosa.

## Ports & Adapters

**Decisão:** hexagonal — a dependência aponta para dentro.

**Recusado:** organização em camadas com o domínio dependendo de ORM/repositório
concreto.

**Por quê:** o domínio só pode ser testado sem infraestrutura se ele **não
conhecer** a infraestrutura. 399 testes de topo (778 subtests) rodam sem stack por causa
disso. E trocar a infraestrutura (LocalStack por AWS, Postgres por outro store)
não toca a regra de negócio.

## Um binário com três modos

**Decisão:** `cmd/walletd` sobe em `all`, `api` ou `worker`.

**Recusado:** três binários, ou apenas `all`.

**Por quê:** três binários duplicariam a montagem do grafo e divergiriam com o
tempo. Só `all` impede separar carga de API e carga de worker. Um binário com
modo dá os dois: a **mesma** imagem, o **mesmo** grafo, com roles escolhidas por
variável.

| Modo | Para quê |
| :-- | :-- |
| `all` | Desenvolvimento e o ambiente de compose. |
| `api` | Escalar HTTP sem escalar consumidores. |
| `worker` | Escalar consumidores sem multiplicar réplicas de API. |

## O tofu opcional ao lado do boto

**Decisão:** as filas têm duas declarações — o script boto3 que provisiona o
LocalStack e um módulo OpenTofu — e um **teste de sincronia** entre elas.

**Recusado:** substituir o boto pelo tofu (exigiria toolchain no host e quebraria
`make up`).

**Por quê:** `make up` é auto-suficiente hoje, e essa propriedade vale mais do
que ter uma única fonte de verdade. O tofu existe para provar que a declaração
vale fora do LocalStack — onde, medido, as *resource policies* nem são aplicadas.
O teste de sincronia fecha o único risco real da duplicação (as duas metades
divergirem em silêncio).

> [!WARNING]
> **A armadilha que está documentada, não escondida**
>
> O tofu casa recursos por **identidade** no state. Aplicar contra um LocalStack
> já povoado colide em `QueueAlreadyExists`. A saída é `PREFIX=` para uma sonda
> paralela ou `tofu import`. Ver [Infraestrutura como código](../operations/infrastructure.md).


## Verificação de carga em alvo separado

**Decisão:** `make load-verify` (invariantes em SQL) é um alvo **separado** de
`make load` (k6).

**Recusado:** colocar a verificação dentro dos cenários k6.

**Por quê:** o k6 é um cliente HTTP — ele **não vê o Postgres**. As invariantes
contábeis (saldo igual à soma do ledger, ausência de efeitos pela metade) só
existem na camada SQL. Forçar a verificação dentro do k6 daria uma prova pela
metade: o script poderia medir que a API respondeu `202` sem nunca saber se o
dinheiro foi para o lugar certo.

| Instrumento | O que prova |
| :-- | :-- |
| k6 | Aguentou a carga e respondeu. |
| `invariants.sql` | O que ficou gravado está correto. |

Veja também: [Convenções](conventions.md) ·
[Invariantes](../architecture/invariants.md) ·
[Teste de carga](../operations/load-testing.md) ·
[Infraestrutura como código](../operations/infrastructure.md).
