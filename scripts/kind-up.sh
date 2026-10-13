#!/usr/bin/env bash
#
# Creates the kind cluster (idempotent).
#
# Reusing an existing cluster is the default because the slow, stateful part is
# the node image and the registry cache; recreating it on every run would make
# `make kind-deploy` unusable as an inner loop. RECREATE=1 forces a clean slate
# (and therefore a clean database) when that is what you actually want.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-wallet}"
RECREATE="${RECREATE:-0}"
CONFIG="${CONFIG:-deploy/kubernetes/kind/cluster.yaml}"

command -v kind >/dev/null 2>&1 || { echo "kind-up: kind not found; run 'make kind-check'" >&2; exit 1; }
[[ -f "$CONFIG" ]] || { echo "kind-up: cluster config not found: $CONFIG" >&2; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
	if [[ "$RECREATE" == "1" ]]; then
		echo "kind-up: RECREATE=1, deleting existing cluster '$CLUSTER_NAME'"
		kind delete cluster --name "$CLUSTER_NAME"
	else
		# An existing cluster is reused, but `kind create cluster --config` is a
		# no-op against one — so editing cluster.yaml (a new port mapping, for
		# instance) silently does nothing and the deploy keeps talking to the
		# old topology. That failure is genuinely confusing: pods are healthy,
		# the Service is right, and the host port answers with the *wrong
		# service* — which is exactly what happened here, with host 8080 and
		# 8081 pointing at each other's NodePort.
		#
		# So compare the live host->container port mappings against the config
		# and say so loudly when they drift, instead of proceeding as if the
		# file had been applied.
		node="${CLUSTER_NAME}-control-plane"
		drift=0
		if command -v jq >/dev/null 2>&1; then
			live="$(podman inspect "$node" --format '{{json .NetworkSettings.Ports}}' 2>/dev/null || true)"
			[[ -z "$live" ]] && live="$(docker inspect "$node" --format '{{json .NetworkSettings.Ports}}' 2>/dev/null || true)"
			# Desired pairs "hostPort:containerPort" pulled straight out of the config.
			desired="$(sed -n 's/.*{ *containerPort: *\([0-9]*\), *hostPort: *\([0-9]*\).*/\2:\1/p' "$CONFIG" | sort)"
			actual="$(printf '%s' "$live" | jq -r 'to_entries[]? | select(.value != null) | .value[] | "\(.HostPort):\(.value // "")"' 2>/dev/null \
				| sed 's/.*://' | sort -u || true)"
			if [[ -n "$desired" ]]; then
				while IFS= read -r pair; do
					[[ -z "$pair" ]] && continue
					host="${pair%%:*}"; cport="${pair##*:}"
					# `podman port` reports containerPort -> host, so look it up that way.
					if ! podman port "$node" "$cport" 2>/dev/null | grep -q ":${host}$" \
						&& ! docker port "$node" "$cport" 2>/dev/null | grep -q ":${host}$"; then
						echo "kind-up: WARNING: host port $host is not mapped to container port $cport on '$node'" >&2
						drift=1
					fi
				done <<<"$desired"
			fi
		fi
		echo "kind-up: cluster '$CLUSTER_NAME' already exists (RECREATE=1 to rebuild)"
		if [[ "$drift" == "1" ]]; then
			cat >&2 <<EOF

kind-up: the running cluster does not match $CONFIG.
         'kind create cluster --config' cannot update an existing cluster, so
         the config on disk was never applied. Host ports will answer with the
         wrong service. Recreate it:

             RECREATE=1 ./scripts/kind-up.sh
             # or: make kind-up RECREATE=1 ; make kind-deploy
EOF
			exit 1
		fi
		kubectl cluster-info --context "kind-$CLUSTER_NAME" >/dev/null
		exit 0
	fi
fi

echo "kind-up: creating cluster '$CLUSTER_NAME'"
kind create cluster --name "$CLUSTER_NAME" --config "$CONFIG" --wait 120s

# The kubelet registers the node before the CNI is fully programmed; waiting for
# a Ready node avoids racing straight into a deploy that would report
# ImagePullBackOff/CreateContainerError for reasons that have nothing to do
# with the workload.
kubectl wait --context "kind-$CLUSTER_NAME" \
	--for=condition=Ready node --all --timeout=180s

echo "kind-up: cluster ready"
kubectl --context "kind-$CLUSTER_NAME" get nodes
