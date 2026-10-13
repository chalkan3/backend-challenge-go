# pgAudit

Extensão do PostgreSQL que registra no log do **servidor** todo statement que
escreve. Profile `audit` do `docker-compose.yml` — o ambiente do dia a dia
continua na `postgres:17-alpine`, sem extensão.

| Arquivo | Papel |
| :-- | :-- |
| `Dockerfile` | `postgres:17` + `postgresql-17-pgaudit`, com o `include` no `postgresql.conf` |
| `pgaudit.conf` | as diretivas de auditoria (única fonte de verdade) |

```sh
make up-audit
psql "postgres://wallet:wallet@localhost:5434/wallet" \
     -c 'UPDATE wallet_ledger_entries SET amount_minor = 0;'
docker compose --profile audit logs postgres-audit | grep AUDIT
```

## Por que uma imagem custom

`postgres:17-alpine`, usada no dia a dia, **não traz o pgAudit** — nem o `.so`
nem o control file (verificado). O pgAudit entra em `shared_preload_libraries`,
ou seja, é carregado no *start* do servidor (registra hooks do executor) e não
pode ser adicionado a quente. Isso obriga a extensão a existir já no build.

Base Debian de propósito: o PGDG publica `postgresql-17-pgaudit` pré-compilado
casando com o major do servidor. No Alpine viraria compilação a partir do source.

## Duas metades, e ambas precisam existir

1. **O `.so` carregado no boot** — `shared_preload_libraries = 'pgaudit'`, na
   imagem. Sem isto, `CREATE EXTENSION pgaudit` falha com "could not open
   extension control file".
2. **A extensão no catálogo** — `CREATE EXTENSION pgaudit`, na migration
   [`00005_pgaudit.sql`](../../internal/migrations/00005_pgaudit.sql). Ela é
   **condicional**: num banco sem a extensão disponível (Alpine, CI, suíte de
   integração) a migration vira no-op e a subida segue. Auditoria é reforço da
   integridade, não pré-requisito de boot.

## Configuração

`pgaudit.log = 'write, ddl, role'` — `read` fica **desligado** de propósito. O
wallet faz um volume enorme de `SELECT` (leitura de saldo, listagem do ledger);
auditar leitura inundaria o log. O que interessa para a integridade do ledger é
*escrita*.

`pgaudit.log_relation = on` — sem isto, o log mostra o SQL mas não a tabela de
forma estruturada, e um `UPDATE` em `wallets` e um em `wallet_ledger_entries`
ficam indistinguíveis fora do texto do statement.

## Defesa em profundidade

O ledger já tem triggers `ledger_no_update`/`ledger_no_delete` que **impedem** a
mutação (`UPDATE 0` / `DELETE 0`). O pgAudit não substitui isso — ele audita a
*tentativa*:

```
LOG:  AUDIT: SESSION,1,1,WRITE,UPDATE,TABLE,public.wallet_ledger_entries,UPDATE wallet_ledger_entries SET amount_minor = 999999
```

O trigger impede; o pgAudit grava quem tentou, quando e contra qual tabela.
