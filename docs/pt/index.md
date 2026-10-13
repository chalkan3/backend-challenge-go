> Documentação do **serviço de carteiras para provedores de jogos**.
> A dificuldade deste projeto não é a aritmética: é garantir que, sob retry de
> rede, mensagens duplicadas, múltiplas réplicas e reinícios, **dinheiro
> nunca seja contado duas vezes nem perdido**. Cada página aqui descreve
> uma dessas garantias e como ela é imposta — no domínio, no banco, ou nos dois.

- **🚀 Começando** — Requisitos, subir a stack com um comando e executar a primeira aposta em minutos.  
  [`getting-started/quickstart`](getting-started/quickstart.md)
- **🏗️ Arquitetura** — Ports & adapters, composição com Uber Fx e o caminho de uma aposta pelo sistema.  
  [`architecture/overview`](architecture/overview.md)
- **🔌 API** — Todos os endpoints, códigos de erro, idempotência e exemplos com curl.  
  [`api/reference`](api/reference.md)
- **📒 Dados** — Schema, ledger append-only, transactional outbox e inbox durável.  
  [`data/ledger`](data/ledger.md)
- **✉️ Mensageria** — SQS FIFO, consumer com deduplicação, relay do outbox e referências pendentes.  
  [`messaging/overview`](messaging/overview.md)
- **🧪 Operação** — Configuração, observabilidade, testes, carga e solução de problemas.  
  [`operations/testing`](operations/testing.md)

## O que este serviço garante

| Garantia | Como é obtida |
| :-- | :-- |
| 🔁 **Idempotência** | Toda operação carrega `Idempotency-Key`; o resultado é persistido e replicado em replay |
| 📒 **Auditabilidade** | Ledger **append-only**: cada movimento é uma linha imutável que soma até o saldo |
| ✉️ **Entrega confiável** | **Transactional outbox** — o evento é gravado na mesma transação do saldo |
| 📥 **Consumo sem duplicação** | **Inbox durável** deduplica por identidade da mensagem |
| 🔒 **Concorrência** | Lock row-level por carteira + `version` otimista; carteiras distintas progridem em paralelo |
| 🧮 **Precisão monetária** | `Money` é um value object em unidades menores (`int64`), nunca `float` |

## Mapa da documentação

```mermaid
flowchart LR
    subgraph Entrada["Quero..."]
        A[Entender o desenho] --> B[Arquitetura]
        C[Rodar o projeto] --> D[Começando]
        E[Chamar a API] --> F[API]
        G[Operar / depurar] --> H[Operação]
    end

    B --> B1[Visão geral]
    B --> B2[Hexagonal]
    B --> B3[Fluxo de uma aposta]
    B --> B4[Modelo de domínio]
    B --> B5[Invariantes]

    D --> D1[Requisitos]
    D --> D2[Subir a stack]
    D --> D3[Primeira aposta]

    F --> F1[Endpoints]
    F --> F2[Erros]
    F --> F3[Idempotência]

    H --> H1[Configuração]
    H --> H2[Observabilidade]
    H --> H3[Testes e carga]

    classDef hub fill:#1d5fa8,color:#fff,stroke:#0b3d2c,stroke-width:2px
    classDef leaf fill:#e8f0fa,color:#0b3d2c,stroke:#1d5fa8
    class A,C,E,G,B,D,F,H hub
    class B1,B2,B3,B4,B5,D1,D2,D3,F1,F2,F3,H1,H2,H3 leaf
```

## Começar em um comando

```sh
make up          # sobe postgres, keycloak, localstack e a app
make test        # suíte rápida (não precisa da stack)
make e2e         # contrato ponta a ponta contra a stack real
```

> [!TIP]
> **Este site é bilíngue**
>
> Use o seletor de idioma no cabeçalho para alternar entre **Português** e
> **English**. O conteúdo é espelhado página a página.


> [!NOTE]
> **O README continua sendo a porta de entrada do repositório**
>
> Este site é documentação detalhada. O [`README.md`](https://github.com/chalkan3/backend-challenge-go)
> na raiz do projeto permanece como visão geral e é o primeiro contato de
> quem clona o repositório.

