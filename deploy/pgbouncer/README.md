# PgBouncer

Pool de conexões em frente ao Postgres. Serviço `pgbouncer` no
`docker-compose.yml`, sob o profile **`pooled`** — ou seja, o `up` normal do
dia a dia continua falando direto com o Postgres e nada muda.

```sh
# sobe com o pooler no meio
docker compose --profile pooled up -d

# o app passa a conectar via pgbouncer:6432 em vez de postgres:5432
docker compose --profile pooled exec app env | grep WALLET_DB_DSN
```

| Arquivo | Papel |
| :-- | :-- |
| `pgbouncer.ini` | configuração do pool (`pool_mode = transaction`) |
| `userlist.txt` | mapa usuário/senha, espelha o Postgres local |

> [!WARNING]
> `userlist.txt` **não aceita linhas de comentário**. O parser é estrito: um `#`
> antes da entrada faz o usuário sumir (`no such user: wallet` → `"trust"
> authentication failed`). O arquivo tem que conter só as linhas `"user" "pass"`.

## Por que `transaction` mode

| Modo | A conexão de servidor é devolvida… | Multiplexa? |
| :-- | :-- | :-- |
| `session` | no fim da **sessão** do cliente | não — vira proxy caro |
| `transaction` | no fim da **transação** | sim — o ganho real |
| `statement` | no fim de cada **statement** | máximo, mas proíbe transações multi-statement |

O wallet usa `WithinTx` para gravar saldo + ledger + inbox/outbox atomicamente.
`statement` mode quebraria isso (cada statement cairia num backend diferente),
então `transaction` é o único dos três que preserva a atomicidade **e** agrupa
conexões.

## Por que o pgx v5 não quebra aqui

O erro clássico de PgBouncer + `transaction` mode é:

```
ERROR: prepared statement "stmtcache_1f2e..." already exists
```

Ocorre porque o driver prepara um statement nomeado numa conexão de servidor, o
PgBouncer devolve essa conexão ao pool, e outro cliente a recebe sem o
statement. `sqlx`, `database/sql` e drivers que fixam `statement_cache_mode`
sofrem.

O **pgx v5 detecta o PgBouncer no handshake** e troca automaticamente para o
protocolo simples, sem statement nomeado persistente. Nada no código do wallet
precisa mudar — e este repositório prova isso com a suíte de integração
completa rodando através do pooler (ver README).
