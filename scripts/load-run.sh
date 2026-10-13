#!/usr/bin/env bash
# Run the k6 load scenarios against a live wallet stack.
#
# k6 itself is not installed on the host: it runs from the official image, the
# same way the rest of the project brings up its tooling (compose, kind). That
# keeps the developer machine clean and pins the k6 version for everyone.
#
# Usage:
#   scripts/load-run.sh smoke
#   scripts/load-run.sh hot
#   scripts/load-run.sh distributed          # needs make up-multi
#   scripts/load-run.sh all
#
# Environment:
#   API    base URL of one instance        (default http://localhost:8081)
#   APIS   comma-separated list for `distributed`
#   K6     k6 image                        (default docker.io/grafana/k6:latest)
#   Extra -e flags are forwarded, e.g.  VUS=128 scripts/load-run.sh hot
set -euo pipefail

cd "$(dirname "$0")/.."

K6_IMAGE="${K6:-docker.io/grafana/k6:latest}"
API="${API:-http://localhost:8081}"
APIS="${APIS:-}"

if ! command -v docker >/dev/null 2>&1; then
  echo "load-run: docker/podman not found on PATH" >&2
  exit 1
fi

# k6 in the container reaches the host through the host network. On Linux with
# Podman/Docker this is the only network mode where localhost means the host.
NET_ARGS=(--network host)

k6_run() {
  local script="$1"
  shift
  docker run --rm "${NET_ARGS[@]}" \
    -v "$PWD/loadtest/k6:/scripts:ro" \
    -w /scripts \
    -e API="$API" \
    ${APIS:+-e APIS="$APIS"} \
    "$@" \
    "$K6_IMAGE" run "/scripts/scenarios/$script"
}

wait_for_api() {
  local url="${1%/}/health/ready"
  for _ in $(seq 1 30); do
    if curl -fsS -m 2 "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "load-run: $url never became ready — is the stack up? (make up)" >&2
  return 1
}

run_smoke()       { echo "### smoke";       k6_run smoke.js; }
run_hot()         { echo "### hot-wallet";  k6_run hot-wallet.js "${@}"; }
run_idempotency() { echo "### idempotency"; k6_run idempotency.js "${@}"; }
run_mixed()       { echo "### mixed";       k6_run mixed.js "${@}"; }
run_distributed() { echo "### distributed"; k6_run distributed.js "${@}"; }
run_soak()        { echo "### soak";        k6_run soak.js "${@}"; }

target="${1:-smoke}"
shift || true

wait_for_api "$API"

case "$target" in
  smoke)       run_smoke ;;
  hot)         run_hot "$@" ;;
  idempotency) run_idempotency "$@" ;;
  mixed)       run_mixed "$@" ;;
  distributed) run_distributed "$@" ;;
  soak)        run_soak "$@" ;;
  all)
    run_smoke
    run_hot
    run_idempotency
    run_mixed
    if [[ -n "$APIS" ]]; then
      run_distributed
    else
      echo "### distributed (skipped: set APIS=http://host:8081,http://host:8082,...)"
    fi
    ;;
  *)
    echo "load-run: unknown target '$target' (smoke|hot|idempotency|mixed|distributed|soak|all)" >&2
    exit 1
    ;;
esac
