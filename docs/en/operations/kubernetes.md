> Alongside `docker-compose`, the project ships a **second way to
> bring the environment up**: a local Kubernetes cluster with
> `kind`, a Helm chart for the service and an optional GitOps path with
> Argo CD. It is an alternative, not a replacement — `make up` remains
> the short path.

## Why two ways to bring it up

| | `docker compose` | Kubernetes (`kind`) |
| :-- | :-- | :-- |
| Use | daily development, integration tests | validate manifests, split, probes, GitOps |
| Target | `make up` | `make kind-deploy` |
| Speed | seconds | minutes (build + image load) |
| Fidelity | high for the app | high for the **orchestration** |

> [!NOTE]
> **Compose is not the second-class path**
>
> `make up` is **self-sufficient**. The Kubernetes path exists to prove the
> service runs under a real orchestrator — probes, api/worker split,
> `ServiceAccount`, ConfigMap/Secret — without that conditioning day-to-day
> work.


## The tree

```
deploy/kubernetes/
├── charts/
│   └── wallet/                 # the service's Helm chart
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── values-gitops.yaml
│       ├── files/              # files mounted into the pod
│       └── templates/
│           ├── deployment-app.yaml     # all mode
│           ├── deployment-split.yaml   # api + worker as separate pods
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
│   └── cluster.yaml            # the local cluster definition
└── argocd/
    └── application.yaml        # App of Apps (optional GitOps)
```

## The scripts

They all live in `scripts/` and are called by the Makefile targets.

| Script | Role |
| :-- | :-- |
| `kind-check.sh` | Checks whether the prerequisites exist (kind, kubectl, helm). Checks, does not assume. |
| `kind-up.sh` | Creates (or recreates) the local cluster from `kind/cluster.yaml`. |
| `kind-image.sh` | Builds the image and loads it into the cluster. |
| `kind-deploy.sh` | Applies the Helm chart; with `SPLIT=1` it separates api and worker. |
| `kind-argocd.sh` | Installs Argo CD into the cluster. |
| `kind-argocd-seed.sh` | Seeds the Git repository Argo CD watches. |
| `kind-status.sh` | Shows the state of pods, services and ingress. |

```sh
# Prerequisite check before anything else.
make kind-check

# Cluster + image + chart, all in one step.
make kind-deploy

# With the observability stack and api/worker separated.
make kind-deploy OBSERVABILITY=1 SPLIT=1
```

## Targets

| Target | What it does |
| :-- | :-- |
| `make kind-check` | Checks kind, kubectl and helm. |
| `make kind-up` | Creates the local cluster. |
| `make kind-image` | Builds and loads the image. |
| `make kind-deploy` | Applies the chart (accepts `OBSERVABILITY=1`, `SPLIT=1`). |
| `make kind-split` | Deployment with api and worker as distinct Deployments. |
| `make kind-argocd` | Installs Argo CD (depends on `kind-deploy`). |
| `make kind-argocd-seed` | Seeds the GitOps repo. |
| `make kind-status` | Cluster state. |
| `make kind-down` | Tears the cluster down. |
| `make helm-lint` | Chart lint, no cluster needed. |

> [!TIP]
> **`make kind-check` first, always**
>
> Kind is **checked, not assumed**. Running `kind-deploy` without `kind`
> installed fails halfway through; `kind-check` fails earlier, with the right
> message.


> [!NOTE]
> **`make helm-lint` needs no cluster**
>
> It is the right target for CI and for validating a chart change before paying
> the cost of bringing a cluster up.


## The split on Kubernetes

The service is **one binary with three modes** (see
[Configuration](configuration.md)). The chart exploits exactly that:

| Deployment | Mode | What runs |
| :-- | :-- | :-- |
| `deployment-app.yaml` | `all` | HTTP + workers in the same pod. |
| `deployment-split.yaml` | `api` and `worker` | Two Deployments, scaled independently. |

That is what lets you scale consumers without multiplying API replicas — the
split is one of **process**, not of code, so both halves share the same image.

> [!WARNING]
> **`SPLIT=1` on the API and on the worker**
>
> With the split on, both Deployments read the same ConfigMap and the same
> Secret, but with a different `WALLET_MODE`. HTTP probes stay valid in the
> `api` pod; the `worker` pod exposes no business routes.


## Optional GitOps

`deploy/kubernetes/argocd/application.yaml` is an *App of Apps*: it points at a
Git repository and reconciles the cluster against what is there. It is
**optional** and demonstrates the declarative loop, not the development
workflow.

```yaml
# Sketch of the App of Apps (the real file lives in deploy/kubernetes/argocd).
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

`values-gitops.yaml` exists separately from `values.yaml` on purpose: what Argo
CD reconciles is not the same thing the developer uses in `kind-deploy`.

```sh
# Full GitOps loop, locally.
make kind-argocd        # installs Argo CD
make kind-argocd-seed   # seeds the watched repo
make kind-status        # checks the reconciliation
```

See also: [Configuration](configuration.md) ·
[Infrastructure as code](infrastructure.md) ·
[Troubleshooting](troubleshooting.md).
