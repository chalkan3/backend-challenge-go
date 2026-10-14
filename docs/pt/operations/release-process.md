> Como uma versão vira um release publicado: quem decide o número, quem cria a
> tag, quem sobe os binários — e a única configuração que faz a corrente toda
> funcionar em vez de falhar em silêncio.

## O problema que isto resolve

Três coisas precisam acontecer para um release existir, e são naturezas
diferentes de ato:

| Ato | Tipo de decisão | Quem |
| :-- | :-- | :-- |
| Decidir a versão (`v0.2.0`) | um julgamento, feito lendo o que mudou | **humano**, num PR |
| Criar a tag | um passo mecânico que não deve ser manual | **release-please** |
| Construir e subir os artefatos | um trabalho longo e determinístico | **GoReleaser** |

Separar assim é deliberado. A tag é a única coisa imutável — e assinável — do
processo. Se um robô infere a versão *e* publica sem revisão, um erro de
inferência vira um artefato publicado que não dá para desfazer honestamente: o
melhor que se consegue é publicar uma versão *seguinte* que não era a
pretendida. Então o robô propõe, o PR é revisado, e o merge é o que autoriza a
tag.

## A corrente, ponta a ponta

```mermaid
flowchart TD
    A[commit no main<br/>feat: / fix: / feat!: ] --> B[release-please.yml]
    B --> C[PR de release<br/>CHANGELOG.md + bump no manifest]
    C -->|humano revisa e mergeia| D[tag v0.2.0<br/>+ GitHub Release]
    D --> E[release.yml<br/>push: tags v*.*.*]
    E --> F[job binaries<br/>GoReleaser --skip=validate<br/>mode: append]
    E --> G[job image<br/>GHCR multi-arch]
    F --> H[binários + checksums + SBOMs<br/>anexados à MESMA Release]
    G --> H
```

As duas pontas dessa corrente são as partes que valem entender, porque cada uma
contém uma armadilha.

### Entrada: o que faz o release-please propor uma versão

O release-please lê os
[Conventional Commits](https://www.conventionalcommits.org/) desde a última tag.
Só alguns tipos mexem no número:

| Commit | Efeito na versão |
| :-- | :-- |
| `fix: …` | **patch** — `0.1.0` → `0.1.1` |
| `feat: …` | **minor** — `0.1.0` → `0.2.0` |
| `feat!: …` ou qualquer commit com footer `BREAKING CHANGE:` | **major** — `0.1.0` → `1.0.0` |
| `docs:`, `test:`, `ci:`, `build:`, `refactor:`, `chore:` | **nada** — nenhum PR de release é aberto |

Essa última linha é o motivo pelo qual este repositório adota a ferramenta sem
cerimônia: o histórico já são 40 commits convencionais, e os 13 `feat` / 4 `fix`
entre eles são exatamente os que deveriam ter movido uma versão.

A versão proposta aparece no **diff do PR de release** — no `CHANGELOG.md` e no
`.release-please-manifest.json`. Esse diff é a revisão. Se a proposta estiver
errada, fecha-se o PR e consertam-se os commits; nada foi tagueado, então nada
foi publicado.

### Saída: a armadilha que faz falhar em silêncio

Aqui está a configuração que decide se tudo isto funciona de fato:

> **Uma tag empurrada com o `GITHUB_TOKEN` padrão não dispara outros
> workflows.**

É uma regra anti-recursão deliberada do GitHub, e ela cai exatamente sobre este
desenho. O `release.yml` escuta `push: tags: ["v*.*.*"]`. Se o release-please
criar a tag com o `GITHUB_TOKEN`, esse push **não** dispara o `release.yml`. A
tag aparece, a Release aparece (criada pelo próprio release-please), e
**nenhum binário é publicado**. Nada fica vermelho. A falha é invisível até
alguém tentar baixar um asset do release.

**A armadilha já foi disparada neste repositório**, e o rastro continua no
histórico — serve de diagnóstico pronto:

```sh
gh run list --workflow=release.yml
#  success  push  v0.1.0          ← disparou: a tag foi empurrada por outra identidade

gh release view walletd-v0.2.0 --json assets
# {"assets":[]}                  ← a release existe, o binário não
```

A `v0.2.0` é o caso negativo: o `release.yml` **nunca rodou** para ela (não há
run correspondente), enquanto a `v0.1.0` tem um run `success`. A diferença não
está no workflow nem no código — está em **quem** empurrou a tag. A comparação
vale mais que o aviso, porque é o estado real que uma configuração errada deixa
para trás: duas Releases na mesma página, uma com assets, outra sem, e nada em
vermelho para explicar por quê.

A correção é um token que **não** seja o `GITHUB_TOKEN`: um token de outra
identidade está fora da regra anti-recursão, e o push de tag que ele faz dispara
workflows normalmente. Há dois caminhos aceitos, com um trade-off explícito:

| | GitHub App | PAT fine-grained |
| :-- | :-- | :-- |
| Secrets | `RELEASE_PLEASE_APP_ID` + `RELEASE_PLEASE_APP_PRIVATE_KEY` | `RELEASE_PLEASE_TOKEN` |
| Identidade | o App, não uma pessoa | a pessoa que criou o PAT |
| Permissões | por instalação, só as que o App pediu | escopadas a **um** repositório |
| Validade | token de 1h, gerado a cada run | a validade do PAT (expira sozinho) |
| Rotação | a chave privada, raramente | o PAT, antes de expirar |
| Como criar | pelo navegador | por CLI (`gh api`) |

**Um PAT fine-grained não é obrigado a ser um PAT clássico.** O clássico tem
escopos amplos (`repo` cobrindo tudo que a conta enxerga); o fine-grained é
escopado a *este* repositório, com as permissões mínimas e validade obrigatória.
É o que o torna aceitável: o raio de dano de vazar o secret é um repositório, e
ele expira sozinho. O que ele não tem é o melhor dos dois mundos — ainda é um
secret de vida longa pertencente a uma pessoa, e é isso que um App resolve. Neste
repositório o PAT é o caminho escolhido, porque criar um App exige interação no
navegador e `gh` não expõe essa API; o App continua suportado pelo workflow e
volta a ter precedência se for configurado.

### Pré-requisito: deixar o Actions abrir PRs

Há um segundo interruptor, além do token, e ele falha **antes** do primeiro —
por isso é o primeiro a checar quando o workflow fica vermelho:

```
##[error]release-please failed: GitHub Actions is not permitted to create or
approve pull requests.
```

O sintoma é enganoso: o log mostra o release-please lendo os commits, montando o
PR e atualizando a referência da branch — **todo o trabalho feito** — e só então
morrendo na chamada `POST /pulls`. Nada do seu projeto está errado; a permissão
padrão do repositório é que não deixa o `GITHUB_TOKEN` criar PRs.

**Não precisa do navegador.** A configuração é exposta pela API, então cabe em
um comando:

```sh
gh api repos/chalkan3/backend-challenge-go/actions/permissions/workflow
# {"default_workflow_permissions":"read","can_approve_pull_request_reviews":false}
#                                                                        ^^^^^ o culpado

gh api --method PUT repos/chalkan3/backend-challenge-go/actions/permissions/workflow \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=true
```

O `default_workflow_permissions=read` é deliberado e **deve ficar como está**: o
`can_approve_pull_request_reviews` é o que autoriza o PR, e a permissão `read`
como padrão mantém todo workflow sem `permissions:` explícito incapaz de escrever
no repositório. Este repositório declara o que precisa, workflow por workflow
(`contents: write` no release-please, `packages: write` no release), em vez de
pedir escrita global.

> [!IMPORTANT]
> **Este interruptor e o token são problemas independentes, e os dois aparecem.**
> Corrigir a permissão faz o PR abrir; **não** faz a tag disparar o `release.yml`.
> Sem um App ou um PAT configurado, o workflow cai no `GITHUB_TOKEN`, emite o
> aviso alto no fim do log e cria uma tag **sem binário publicado**. O sintoma
> dessa segunda falha é mais cruel, porque não fica vermelho: só aparece ao
> tentar baixar um asset de uma Release que existe.

### Configurando o PAT fine-grained

O PAT precisa de duas permissões de repositório, e de nada além disso:

| Permissão | Nível | Por que |
| :-- | :-- | :-- |
| **Contents** | Read and write | criar a tag e a Release |
| **Pull requests** | Read and write | abrir/atualizar o PR de release |

```sh
# 1. crie o PAT no navegador, no dono do repositório:
#    https://github.com/settings/personal-access-tokens/new
#    Repository access: Only select repositories → backend-challenge-go
#    Permissions: Contents → Read and write · Pull requests → Read and write

# 2. grave-o como secret (o valor é lido da stdin, não fica no histórico):
gh secret set RELEASE_PLEASE_TOKEN --repo chalkan3/backend-challenge-go
```

Se `gh` estiver autenticado como outro usuário, passe `--repo` explicitamente em
cada chamada — o alvo do secret não é inferido do diretório quando você usa
`--repo`. Um PAT fine-grained **expira**: o secret não falha em silêncio quando
isso acontece, e é justamente aí que este workflow é útil, porque ele emite o
aviso descrito abaixo em vez de publicar uma tag órfã.

### Configurando o GitHub App (alternativa)

1. Crie um GitHub App com **Contents: read & write** e
   **Pull requests: read & write**.
2. Instale-o neste repositório.
3. Adicione os secrets **`RELEASE_PLEASE_APP_ID`** e
   **`RELEASE_PLEASE_APP_PRIVATE_KEY`**.

### A precedência, e o aviso

O `release-please.yml` resolve o token em ordem — **App → PAT →
`GITHUB_TOKEN`** — e usa o primeiro que existir. Se os secrets do App estiverem
presentes, ele gera um token com `actions/create-github-app-token` e ignora o
PAT. Se nenhum dos dois existir, ele cai para o `GITHUB_TOKEN` **e emite um
aviso alto** — não falha, porque um release só de tag e changelog ainda é útil.
Mas o aviso é o sinal de que a metade binária do release não está rodando.

> [!IMPORTANT]
> Enquanto nem `RELEASE_PLEASE_APP_ID`/`RELEASE_PLEASE_APP_PRIVATE_KEY` nem
> `RELEASE_PLEASE_TOKEN` existirem, o fluxo automatizado produz tags e
> changelogs, mas **nenhum binário**. O caminho `workflow_dispatch` do
> `release.yml` não é afetado e continua servindo para cortar um release na mão.

## Os dois workflows não brigam pela Release

Os dois workflows tocam a mesma GitHub Release, então a divisão precisa ser
explícita:

| | release-please | GoReleaser (via `release.yml`) |
| :-- | :-- | :-- |
| Cria a Release | **sim** | não — `mode: append` |
| Cria a tag | **sim** | não |
| Escreve o changelog | **sim** (a partir dos commits) | não |
| Sobe binários, checksums e SBOMs | não | **sim** |

Como o release-please cria a Release *antes* do `release.yml` rodar, o
GoReleaser sempre está anexando a uma Release que já existe — é por isso que o
`release.yml` passa `--skip=validate` e o `.goreleaser.yml` define
`release.mode: append`. Sem os dois, o GoReleaser aborta com
`release already exists` — e abortaria **só** no caminho automatizado, nunca
num dispatch manual com tag nova, que é o caminho que se testa primeiro. É isso
que torna a combinação fácil de errar.

A Release final tem, portanto, duas contribuições: uma seção narrativa (o
changelog gerado) e uma seção de arquivos (os tarballs, o `checksums.txt`, os
SBOMs e a saída de `walletd version`).

## Fazendo um release, na prática

**Fluxo normal** — não há nada a fazer além de mergear:

```sh
# 1. commits convencionais no main
git commit -m "feat(api): expõe o journal do ledger"
git push

# 2. o release-please abre (ou atualiza) um PR "chore(main): release 0.2.0"
# 3. revise o diff do CHANGELOG, depois mergeie esse PR
# 4. a tag, a Release e os binários acontecem a partir daí
```

**Release manual** — para um hotfix ou para bootstrap da primeiríssima tag:

```sh
gh workflow run release.yml -f tag=v0.1.0
```

**Verificando um release** — a partir da própria página do release:

```sh
sha256sum -c checksums.txt
tar xzf walletd_0.2.0_linux_amd64.tar.gz
./walletd version     # imprime a tag, o commit e a data gravados no link
```

## Bootstrap do primeiro release

O release-please precisa de um ponto de partida. Sem nenhuma tag no
repositório, ele assume `0.0.0` — o que não é nocivo, mas o primeiro PR de
release é muito mais fácil de confiar se houver uma tag real por trás. E isso
também prova que a corrente do `release.yml` funciona *antes* de automatizar em
cima dela.

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Essa tag dispara o `release.yml` diretamente (uma tag empurrada na mão não está
sujeita à regra de recursão do `GITHUB_TOKEN`), que é a forma mais limpa de
confirmar que binários, checksums e SBOMs chegam todos à Release. Só então o
`release-please` é apontado para um repositório cujo mecanismo de release sabe-se
que funciona.

## Relacionados

- [`pt/operations/kubernetes.md`](kubernetes.md) — a imagem que o release
  publica, e como ela é implantada.
- [`pt/operations/configuration.md`](configuration.md) — `WALLET_VERSION` e a
  build info que o binário reporta.
- [`pt/contributing/conventions.md`](../contributing/conventions.md) — a
  convenção de mensagem de commit, que é a *entrada* de todo este processo.
