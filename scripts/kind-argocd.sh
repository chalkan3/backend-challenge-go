#!/usr/bin/env bash
#
# GitOps path: installs ArgoCD into the cluster and registers the Application
# that points at the in-cluster git host (see the header of the chart's
# argocd-gitea.yaml for why a git host and not a local path).
#
# ArgoCD is installed with Helm from its published chart, the same way it would
# be for a real cluster; only the source repo is local-specific.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-wallet}"
ARGOCD_VERSION="${ARGOCD_VERSION:-7.7.11}"   # chart version
NAMESPACE="${NAMESPACE:-default}"

cd "$(dirname "$0")/.."

command -v helm >/dev/null 2>&1 || { echo "kind-argocd: helm not found" >&2; exit 1; }
kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

# 1. ArgoCD itself -----------------------------------------------------------
echo "kind-argocd: installing ArgoCD (chart $ARGOCD_VERSION)"
helm repo add argo https://argoproj.github.io/argo-helm >/dev/null 2>&1 || true
helm repo update argo >/dev/null

helm upgrade --install argocd argo/argo-cd \
	--namespace argocd --create-namespace \
	--version "$ARGOCD_VERSION" \
	--set configs.params."server\.insecure"=true \
	--set server.service.type=NodePort \
	--set server.service.nodePortHttp=30090 \
	--wait --timeout 5m

# 2. The git host + the seeded repo -----------------------------------------
# argocd.enabled renders Gitea and its PVC from the chart itself, which keeps
# the whole GitOps story described in one place.
CLUSTER_NAME="$CLUSTER_NAME" ./scripts/kind-argocd-seed.sh "$CLUSTER_NAME"

# 3. The Application ---------------------------------------------------------
echo "kind-argocd: applying Application"
kubectl apply -f deploy/kubernetes/argocd/application.yaml

echo
echo "kind-argocd: ArgoCD UI -> http://localhost:8090  (admin)"
echo "  initial password: kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo"
echo "  status:           kubectl -n argocd get applications"
