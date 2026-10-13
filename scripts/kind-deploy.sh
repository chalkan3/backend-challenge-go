#!/usr/bin/env bash
#
# One-command local Kubernetes environment: cluster + image + chart, the
# equivalent of `docker compose up --build`.
#
#   ./scripts/kind-deploy.sh                 # Postgres + Keycloak + LocalStack + app
#   SPLIT=1 ./scripts/kind-deploy.sh         # api + workers (WALLET_MODE)
#   OBSERVABILITY=1 ./scripts/kind-deploy.sh # + collector/tempo/loki/prom/grafana
#
# The chart already knows how to reach its dependencies inside the cluster
# (Service DNS, the same wiring compose expresses with depends_on + service
# names), so there is no orchestration to do here beyond ordering: the
# dependencies must exist before the app's migrations run.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-wallet}"
NAMESPACE="${NAMESPACE:-default}"
RELEASE_NAME="${RELEASE_NAME:-wallet}"
SPLIT="${SPLIT:-0}"
OBSERVABILITY="${OBSERVABILITY:-0}"
RECREATE="${RECREATE:-0}"
TIMEOUT="${TIMEOUT:-5m}"
VALUES="${VALUES:-}"
SET="${SET:-}"
CHART="${CHART:-deploy/kubernetes/charts/wallet}"

cd "$(dirname "$0")/.."

# 1. Preflight ---------------------------------------------------------------
./scripts/kind-check.sh

# 2. Cluster -----------------------------------------------------------------
CLUSTER_NAME="$CLUSTER_NAME" RECREATE="$RECREATE" ./scripts/kind-up.sh

# 3. Image (build locally, load into the node) -------------------------------
CLUSTER_NAME="$CLUSTER_NAME" ./scripts/kind-image.sh
# .kind-image is a KEY=VALUE file written by kind-image.sh: the image name the
# engine actually stored is only known to the engine, so it is read back there
# and consumed here instead of being re-derived from the tag.
IMAGE_FILE=".kind-image"
[[ -f "$IMAGE_FILE" ]] || { echo "kind-deploy: $IMAGE_FILE missing after kind-image" >&2; exit 1; }
IMAGE_REPOSITORY=""
IMAGE_TAG=""
while IFS='=' read -r k v; do
	case "$k" in
	IMAGE_REPOSITORY) IMAGE_REPOSITORY="$v" ;;
	IMAGE_TAG) IMAGE_TAG="$v" ;;
	esac
done < "$IMAGE_FILE"
[[ -n "$IMAGE_REPOSITORY" && -n "$IMAGE_TAG" ]] || {
	echo "kind-deploy: could not parse $IMAGE_FILE" >&2; cat "$IMAGE_FILE" >&2; exit 1;
}

# 4. Chart -------------------------------------------------------------------
# Everything the environment needs is expressed as --set here, so the chart's
# values.yaml keeps being the documented default and this script keeps being the
# record of what "local kind" means.
sets=(
	--set "image.repository=${IMAGE_REPOSITORY}"
	--set "image.tag=${IMAGE_TAG}"
	--set "split.enabled=$([[ "$SPLIT" == "1" ]] && echo true || echo false)"
	--set "observability.enabled=$([[ "$OBSERVABILITY" == "1" ]] && echo true || echo false)"
	--set "observability.otel.enabled=$([[ "$OBSERVABILITY" == "1" ]] && echo true || echo false)"
)

# Extra values file (e.g. VALUES=deploy/kubernetes/environments/local/kind.yaml).
if [[ -n "$VALUES" ]]; then
	[[ -f "$VALUES" ]] || { echo "kind-deploy: values file not found: $VALUES" >&2; exit 1; }
	sets+=(--values "$VALUES")
fi
# Free-form overrides: SET='ingress.enabled=true,service.exposeNodePorts=false'
if [[ -n "$SET" ]]; then
	IFS=',' read -r -a extra <<<"$SET"
	for e in "${extra[@]}"; do sets+=(--set "$e"); done
fi

echo "kind-deploy: installing release '$RELEASE_NAME' in namespace '$NAMESPACE'"
helm upgrade --install "$RELEASE_NAME" "$CHART" \
	--namespace "$NAMESPACE" --create-namespace \
	--wait --timeout "$TIMEOUT" \
	"${sets[@]}"

# 5. Report ------------------------------------------------------------------
echo
echo "kind-deploy: workloads"
kubectl -n "$NAMESPACE" get pods,svc -l "app.kubernetes.io/instance=$RELEASE_NAME" -o wide

cat <<EOF

kind-deploy: ready. Endpoints on the host (kind extraPortMappings):
  API       http://localhost:8081
$( [[ "$SPLIT" == "1" ]] && echo "  API split http://localhost:8084
  workers   http://localhost:8085" )
  Keycloak  http://localhost:8080
  LocalStack http://localhost:4566
$( [[ "$OBSERVABILITY" == "1" ]] && echo "  Grafana   http://localhost:30300
  Prometheus http://localhost:30909" )

  make e2e                # the same integration suite, against this cluster
  WALLET_TEST_API_URL=http://localhost:8081 make e2e
  make kind-status / make kind-down
EOF
