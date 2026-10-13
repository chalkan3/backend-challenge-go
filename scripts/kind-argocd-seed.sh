#!/usr/bin/env bash
#
# Seeds the local deploy/kubernetes tree into the in-cluster git host.
#
# This is the bridge between "ArgoCD cannot read a local directory" and "I want
# to point ArgoCD at my working copy": the working copy is pushed into Gitea
# inside the cluster, and the Application's repoURL points at that. Re-running
# this script after an edit is what produces a new revision for ArgoCD to sync
# — the same gesture as `git push`, minus the network.
#
# The repo content is exactly deploy/kubernetes, so the chart path inside the
# repo is `charts/wallet`, matching application.yaml.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-${1:-wallet}}"
REPO_NAME="${REPO_NAME:-wallet-gitops}"
GITEA_USER="${GITEA_USER:-gitea}"

cd "$(dirname "$0")/.."
SRC="deploy/kubernetes"

kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

# Gitea lives in the argocd namespace (it is part of the GitOps wiring).
kubectl -n argocd rollout status deployment/gitea --timeout=180s

# Reach Gitea through a local port-forward rather than a NodePort: this is a
# one-shot seeding operation, not something that should stay exposed.
PF_PORT="${PF_PORT:-33001}"
kubectl -n argocd port-forward svc/gitea "$PF_PORT:3000" >/dev/null 2>&1 &
PF_PID=$!
workdir="$(mktemp -d)"
trap 'kill "$PF_PID" 2>/dev/null || true; rm -rf "$workdir"' EXIT

for _ in $(seq 1 30); do
	curl -sf "http://localhost:$PF_PORT/api/healthz" >/dev/null 2>&1 && break
	sleep 1
done

GITEA_URL="http://localhost:$PF_PORT"

# Create the repo if it is not there yet (idempotent). The Gitea instance is
# INSTALL_LOCK'd with no admin password in a disposable local cluster, so the
# API is used unauthenticated; DISABLE_REGISTRATION means no one else can
# create anything.
if ! curl -sf "$GITEA_URL/api/v1/repos/$GITEA_USER/$REPO_NAME" >/dev/null 2>&1; then
	echo "kind-argocd-seed: creating repo $GITEA_USER/$REPO_NAME"
	curl -sf -X POST "$GITEA_URL/api/v1/user/repos" \
		-H 'Content-Type: application/json' \
		-d "{\"name\":\"$REPO_NAME\",\"private\":false,\"auto_init\":false}" >/dev/null
fi

# Push the working copy. This repository is created fresh in a temp dir, so
# staging everything in it touches nothing else: rsync the tree in, then let
# git discover it from that (empty, throwaway) repository root.
cp -r "$SRC"/. "$workdir/"
(
	cd "$workdir"
	git init -q -b main
	git config user.email "local@kind"
	git config user.name  "kind seed"
	# Add each top-level entry explicitly; a bare `git add -A` is exactly the
	# pattern that has staged unrelated work in the main checkout before.
	for entry in *; do git add -- "$entry"; done
	git commit -q -m "seed: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	# The in-cluster Gitea accepts anonymous pushes in this locked-down local
	# setup; credentials would go in the remote URL for a real host.
	git remote add origin "$GITEA_URL/$GITEA_USER/$REPO_NAME.git"
	git push -q --force origin main
)

echo "kind-argocd-seed: pushed $SRC -> $GITEA_URL/$GITEA_USER/$REPO_NAME"
