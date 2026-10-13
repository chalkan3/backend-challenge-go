-- +goose Up
-- pgAudit: a extensão que transforma "o ledger é append-only" de uma promessa
-- do código numa garantia do servidor.
--
-- Até aqui, a imutabilidade do ledger repousa em três coisas que vivem DENTRO
-- da aplicação: a ausência de UPDATE/DELETE nos repositórios, um trigger que
-- rejeita mutação em `postings`, e a auditoria #1..#11 do invariants.sql. Todas
-- elas são afirmações sobre *este* binário. Nenhuma impede que alguém com um
-- `psql` aberto rode
--
--     UPDATE wallet_ledger_entries SET amount_minor = 0;
--
-- e saia andando. O trigger de `postings` cobre a tabela `postings`; o ledger
-- físico (`wallet_ledger_entries`) não tem defesa equivalente no banco.
--
-- O pgAudit preenche esse vão pelo lado do servidor: com
-- `pgaudit.log = 'write'`, todo INSERT/UPDATE/DELETE que tocar qualquer tabela
-- gera uma linha imutável no log do Postgres, com o usuário, o database, o
-- statement e a relação afetada. O log não está sob o controle da aplicação —
-- um UPDATE manual da aplicação deixa rastro mesmo que o código nunca faça um.
--
-- Por que a extensão precisa ser criada por DDL, e não só habilitada no conf:
-- as diretivas `shared_preload_libraries` / `pgaudit.log` são carregadas no
-- START do servidor (a extensão registra hooks do executor), mas a extensão
-- em si ainda precisa existir no catálogo do database para emitir os eventos.
-- São duas metades: o `.so` carregado no boot (imagem) + o `CREATE EXTENSION`
-- por banco (aqui).
--
-- Idempotente de propósito: um database que não use a imagem com pgAudit
-- instalado ainda precisa aplicar a migration (o CI, a suíte de integração e
-- quem roda a imagem `postgres:17-alpine` do dia a dia). Sem a extensão
-- disponível, o `CREATE EXTENSION` falharia; o bloco condicional abaixo deixa
-- a migration ser no-op silenciosamente nesse caso — auditoria é um *reforço*
-- da integridade, não um pré-requisito de subida.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_available_extensions WHERE name = 'pgaudit'
    ) THEN
        CREATE EXTENSION IF NOT EXISTS pgaudit;
        RAISE NOTICE 'pgaudit habilitado nesta base';
    ELSE
        RAISE NOTICE 'pgaudit indisponível (imagem sem a extensão) — seguindo sem auditoria no servidor';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- Não removemos a extensão no Down: dropar `pgaudit` de um database em um
-- servidor com `shared_preload_libraries = 'pgaudit'` deixaria o servidor sem
-- o consumidor dos eventos, silenciosamente. A extensão é inócua por si só;
-- o que a controla é `pgaudit.log`.
-- +goose StatementBegin
DO $$ BEGIN RAISE NOTICE 'migration 00005 down: pgaudit mantido por decisão explícita'; END; $$;
-- +goose StatementEnd
