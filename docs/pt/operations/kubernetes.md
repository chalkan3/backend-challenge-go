> Além do `docker-compose`, o projeto traz uma **segunda forma de
> subir** o ambiente: um cluster Kubernetes local com `kind`, um
> chart Helm do serviço e um caminho GitOps opcional com Argo CD. É uma
> alternativa, não um substituto — `make up` continua sendo o caminho
> curto.

## Por que duas formas de subir

| | `docker compose` | Kubernetes (`kind`) |
| :-- | :-- | :-- |
| Uso | desenvolvimento diário, testes de integração | validar manifestos, split, probes, GitOps |
| Alvo | `make up` | `make kind-deploy` |
| Velocidade | segundos | minutos (build + load de imagem) |
| Fidelidade | alta para a app | alta para a **orquestração** |

> [!NOTE]
> **O compose não é o caminho de segunda classe**
>
> `make up` é **auto-suficiente**. O caminho Kubernetes existe para provar que
> o serviço roda sob um orquestrador real — probes, split api/worker,
> `ServiceAccount`, ConfigMap/Secret — sem que isso condicione o dia a dia.


## A árvore

```
deploy/kubernetes/
├── charts/
│   └── wallet/                 # chart Helm do serviço
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── values-gitops.yaml
│       ├── files/              # arquivos montados no pod
│       └── templates/
│           ├── deployment-app.yaml     # modo all
│           ├── deployment-split.yaml   # api + worker separados
│           ├── service-app.yaml
│           ├── service-split.yaml
│           ├── configmap.yaml
│           ├── secret.yaml
│           ├── ingress.yaml
│           ├── serviceaccount.yaml
│           ├── keycloak.yaml
│           ├── localstack.yaml
│           ├── observability.yaml
│           └── argocd-gitea.yaml
├── kind/
│   └── cluster.yaml            # definição do cluster local
└── argocd/
    └── application.yaml        # App of Apps (GitOps opcional)
```

## Os scripts

Todos vivem em `scripts/` e são chamados pelos alvos do Makefile.

| Script | Papel |
| :-- | :-- |
| `kind-check.sh` | Verifica se os pré-requisitos existem (kind, kubectl, helm). Checa, não presume. |
| `kind-up.sh` | Cria (ou recria) o cluster local a partir de `kind/cluster.yaml`. |
| `kind-image.sh` | Constrói a imagem e a carrega no cluster. |
| `kind-deploy.sh` | Aplica o chart Helm; com `SPLIT=1` separa api e worker. |
| `kind-argocd.sh` | Instala o Argo CD no cluster. |
| `kind-argocd-seed.sh` | Semeia o repositório Git que o Argo CD observa. |
| `kind-status.sh` | Mostra o estado de pods, serviços e ingress. |

```sh
# Checagem de pré-requisitos antes de qualquer coisa.
make kind-check

# Cluster + imagem + chart, tudo em um passo.
make kind-deploy

# Com stack de observabilidade e api/worker separados.
make kind-deploy OBSERVABILITY=1 SPLIT=1
```

## Alvos

| Alvo | O que faz |
| :-- | :-- |
| `make kind-check` | Verifica kind, kubectl e helm. |
| `make kind-up` | Cria o cluster local. |
| `make kind-image` | Constrói e carrega a imagem. |
| `make kind-deploy` | Aplica o chart (aceita `OBSERVABILITY=1`, `SPLIT=1`). |
| `make kind-split` | Implantação com api e worker em Deployments distintos. |
| `make kind-argocd` | Instala o Argo CD (depende de `kind-deploy`). |
| `make kind-argocd-seed` | Semeia o repo GitOps. |
| `make kind-status` | Estado do cluster. |
| `make kind-down` | Derruba o cluster. |
| `make helm-lint` | Lint do chart, sem cluster. |

> [!TIP]
> **`make kind-check` primeiro, sempre**
>
> Kind é **checado, não presumido**. Rodar `kind-deploy` sem `kind` instalado
> falha no meio do caminho; o `kind-check` falha antes, com a mensagem certa.


> [!NOTE]
> **`make helm-lint` não precisa de cluster**
>
> É o alvo certo para CI e para validar uma mudança no chart antes de pagar o
> custo de subir um cluster.


## O split em Kubernetes

O serviço é **um binário com três modos** (ver
[Configuração](configuration.md)). O chart explora exatamente isso:

| Deployment | Modo | O que roda |
| :-- | :-- | :-- |
| `deployment-app.yaml` | `all` | HTTP + workers no mesmo pod. |
| `deployment-split.yaml` | `api` e `worker` | Dois Deployments, escalados de forma independente. |

É isso que permite escalar consumidores sem multiplicar réplicas de API — a
divisão é de **processo**, não de código, então as duas metades compartilham a
mesma imagem.

> [!WARNING]
> **`SPLIT=1` na API e no worker**
>
> Com o split ligado, os dois Deployments leem o mesmo ConfigMap e o mesmo
> Secret, mas com `WALLET_MODE` diferente. As probes HTTP continuam válidas no
> pod `api`; o pod `worker` não expõe as rotas de negócio.


## GitOps opcional

`deploy/kubernetes/argocd/application.yaml` é um *App of Apps*: ele aponta para
um repositório Git e reconcilia o cluster contra o que está lá. É **opcional** e
serve para demonstrar o ciclo declarativo, não para o fluxo de desenvolvimento.

```yaml
# Esboço do App of Apps (o arquivo real vive em deploy/kubernetes/argocd).
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: wallet
spec:
  destination:
    namespace: wallet
  source:
    path: deploy/kubernetes/charts/wallet
    helm:
      valueFiles:
        - values-gitops.yaml
  syncPolicy:
    automated:
      prune: true
```

O `values-gitops.yaml` existe separado do `values.yaml` de propósito: o que o
Argo CD reconcilia não é o mesmo que o desenvolvedor usa em `kind-deploy`.

```sh
# Ciclo GitOps completo, local.
make kind-argocd        # instala o Argo CD
make kind-argocd-seed   # semeia o repo observado
make kind-status        # confere a reconciliação
```

Veja também: [Configuração](configuration.md) ·
[Infraestrutura como código](infrastructure.md) ·
[Solução de problemas](troubleshooting.md).
