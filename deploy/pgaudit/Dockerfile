# Postgres 17 com a extensão pgAudit.
#
# Por que uma imagem custom: `postgres:17-alpine` (a imagem usada no dia a dia)
# não traz o pgAudit — nem o `.so` nem o control file. Verificado:
#
#   $ docker run --rm postgres:17-alpine \
#       ls /usr/local/lib/postgresql/ | grep -i audit   # vazio
#   $ docker run --rm postgres:17-alpine \
#       ls /usr/local/share/postgresql/extension/ | grep -i audit   # vazio
#
# O pgAudit é um módulo que precisa estar em `shared_preload_libraries` — ou
# seja, carregado no start do servidor, não instalável em runtime. Isso obriga
# a imagem a trazê-lo no build.
#
# Base Debian (não Alpine) de propósito: o repositório PGDG publica binários
# pré-compilados de `postgresql-17-pgaudit` que casam com o major do servidor.
# No Alpine isso viraria compilação a partir do source.
#
#   docker build -f deploy/pgaudit/Dockerfile -t wallet-postgres-audit:17 .
FROM postgres:17

# `postgresql-17-pgaudit` vem do PGDG (já configurado na imagem oficial), na
# versão que casa com o major 17 do servidor.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends postgresql-17-pgaudit; \
    rm -rf /var/lib/apt/lists/*

# Cópia da configuração de auditoria. O arquivo é incluído pelo
# `postgresql.conf` via a flag `-c config_file` ou por conf.d — aqui deixamos
# em /etc/postgresql-audit/ e o compose passa `command: postgres -c ...`.
#
# O caminho é relativo ao CONTEXTO de build (a raiz do repo), não ao
# diretório do Dockerfile — daí o prefixo `deploy/pgaudit/`.
COPY deploy/pgaudit/pgaudit.conf /etc/postgresql-audit/pgaudit.conf

# Sanidade em build time: se a extensão não estiver presente, falha agora e não
# numa subida misteriosa depois.
RUN test -f "/usr/share/postgresql/17/extension/pgaudit.control" \
    || (echo "pgaudit.control ausente — a extensão não foi instalada" && exit 1)

# O `include` precisa morar DENTRO do postgresql.conf que o servidor lê no
# boot — um arquivo montado em outro diretório não é lido sozinho. A imagem
# oficial resolve o CMD como `postgres -c ...`, mas o `config_file` default é
# `$PGDATA/postgresql.conf`; anexar o include ao *sample* que o initdb copia
# garante que toda instância nova já nasça lendo o pgaudit.conf.
#
# `include` (não `include_dir`) porque o arquivo tem nome explícito e o include
# é relativo ao diretório do arquivo incluído se não for absoluto — usamos
# caminho absoluto de propósito, para não depender de onde o PGDATA nascer.
RUN printf "\n# pgAudit — ver deploy/pgaudit/pgaudit.conf\ninclude = '/etc/postgresql-audit/pgaudit.conf'\n" \
    >> /usr/share/postgresql/postgresql.conf.sample
