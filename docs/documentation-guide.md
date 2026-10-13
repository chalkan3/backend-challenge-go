# docs/ — a documentação navegável pelo GitHub

Esta pasta contém a documentação detalhada do serviço de carteiras, em
**português e inglês**, escrita como **Markdown puro** para ser lida **direto no
GitHub** — sem build, sem site, sem dependências, sem workflow.

> [!NOTE]
> **O README da raiz continua sendo a porta de entrada.**
> [`../README.md`](../README.md) é a porta de entrada do projeto e não é
> substituído por esta pasta. Aqui está o material de referência: arquitetura,
> API, dados, mensageria e operação.

---

## Por que Markdown puro

Uma documentação que precisa de `pip install`, de um `mkdocs build` e de um
workflow para virar página tem três pontos de falha entre o leitor e o texto.
Aqui não há nenhum: o GitHub renderiza Markdown nativamente, e esta pasta usa
**apenas** o que ele suporta.

| Recurso | Como é usado |
| :-- | :-- |
| **GitHub Alerts** | `> [!NOTE]`, `> [!TIP]`, `> [!WARNING]` — os blocos destacados |
| **Mermaid** | Diagramas de arquitetura e de fluxo, renderizados pelo próprio GitHub |
| **Tabelas GFM** | Toda comparação, contrato de rota, código de erro |
| **Links relativos** | A navegação entre as páginas (funciona no GitHub, com `.md`) |
| **Emoji** | Leitura rápida das garantias e severidades |

O que **não** é usado, de propósito: CSS customizado, ícones de fontes,
admonitions `!!!` do MkDocs, abas de conteúdo e qualquer HTML além do mínimo.
Nada disso o GitHub renderiza — o que apareceria seria o texto cru da marcação.

---

## Estrutura

```
docs/
├── README.md           # este arquivo — o hub que o GitHub abre em docs/
├── index.md            # home em português
├── en/
│   ├── index.md        # home em inglês
│   └── …               # getting-started, architecture, api, data,
│                       # messaging, operations, contributing
└── pt/                 # a mesma árvore, em português
```

Cada idioma tem a árvore completa e espelhada. As duas homes
([PT](pt/index.md) · [EN](en/index.md)) repetem este índice com links relativos.

---

## Ler no GitHub

Nada a instalar. Abra a pasta `docs/` no navegador: o GitHub renderiza este
`README.md` automaticamente e os links levam às páginas.

Para ler localmente, sem navegador, qualquer renderizador de Markdown serve —
por exemplo:

```sh
# se quiser uma prévia no terminal
glow docs/README.md

# ou simplesmente
less docs/pt/api/reference.md
```

> [!TIP]
> **Ler offline**
> Como são arquivos `.md`, um `git clone` baixa a documentação inteira. Nenhuma
> página depende de rede para ser lida — inclusive os diagramas, que são
> descritos em texto Mermaid.

---

## Escrever nesta documentação

Duas regras mantêm a navegação funcionando:

1. **Links internos apontam para arquivos, com `.md`.** `[Erros](../api/errors.md)`,
   não `../api/errors/`. O GitHub resolve pelo caminho do arquivo.
2. **Toda página nova entra no índice** — no [README](README.md) e na home do
   idioma correspondente ([PT](pt/index.md) · [EN](en/index.md)). Não há
   validação automática: o índice é mantido à mão, e é ele que garante que uma
   página não fique órfã.

> [!WARNING]
> **Não reintroduza HTML customizado**
> Classes como `class="card"`, `class="badge"` ou `<div>` não têm estilo no
> GitHub e aparecem como texto. Use as construções da tabela acima.

---

## Relação com o README da raiz

| | [`README.md`](../README.md) | `docs/` |
| :-- | :-- | :-- |
| **Papel** | Porta de entrada, visão geral do desafio | Referência detalhada |
| **Público** | Quem avalia o projeto | Quem vai trabalhar nele |
| **Mantido** | Conciso, com badges e índice | Exaustivo, uma página por assunto |
