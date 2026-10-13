#!/usr/bin/env bash
# End-to-end runner: brings the real stack up, waits for real healthchecks,
# runs the integration suite (including the journey tests), and tears
# everything down — even if a test fails or you hit Ctrl-C.
#
#   ./scripts/e2e.sh              # full stack + suite (combined app)
#   SPLIT=1 ./scripts/e2e.sh      # two-role topology: WALLET_MODE=api + workers
#   MULTI=1 ./scripts/e2e.sh      # three combined apps (distributed concurrency)
#   ./scripts/e2e.sh -run Journey # pass extra flags straight to `go test`
#   KEEP=1 ./scripts/e2e.sh       # leave the stack running afterwards
#   UP_ONLY=1 ./scripts/e2e.sh    # bring the stack up and stop (no suite), for
#                                 # other tools (k6) to drive it; implies KEEP
#
# Engine/compose detection is centralised in scripts/engine.sh so every entry
# point (this script, the Makefile) agrees on podman vs docker, and a port
# pre-flight catches a kind cluster holding the compose ports.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# shellcheck source=scripts/engine.sh
. scripts/engine.sh
echo "==> using: $COMPOSE (engine: $CONTAINER_ENGINE)"

PROFILE_FLAGS=()
APP_SERVICES=(app)
API_URL_DEFAULT="http://localhost:8081"
if [ "${MULTI:-0}" = "1" ]; then
  PROFILE_FLAGS+=(--profile multi)
  APP_SERVICES+=(app-2 app-3)
fi
# SPLIT=1 exercises the two-role topology (WALLET_MODE=api + WALLET_MODE=workers)
# instead of the combined `app`. Same image, same suite: the point is that the
# API-only process answers traffic while a separate workers process drains the
# queue, proving the mode switch is a real deployment option and not a flag
# that only logs.
if [ "${SPLIT:-0}" = "1" ]; then
  if [ "${MULTI:-0}" = "1" ]; then
    echo "error: SPLIT=1 and MULTI=1 are different topologies; pick one" >&2
    exit 2
  fi
  PROFILE_FLAGS+=(--profile split)
  APP_SERVICES=(api workers)
  API_URL_DEFAULT="http://localhost:8084"
fi

cleanup() {
  if [ "${KEEP:-0}" = "1" ] || [ "${UP_ONLY:-0}" = "1" ]; then
    echo "==> leaving the stack up (tear down with: $COMPOSE ${PROFILE_FLAGS[*]:-} down -v)"
    return
  fi
  echo "==> tearing down"
  $COMPOSE "${PROFILE_FLAGS[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ── 1. Bring the stack up ───────────────────────────────────────────────────
# Fail fast if a kind cluster (or anything else) already owns the host ports;
# otherwise the containers are created but never start, with no clear error.
echo "==> checking host ports"
PROFILES="${SPLIT:+split}${MULTI:+multi}" ./scripts/port-check.sh

echo "==> bringing the stack up"
$COMPOSE "${PROFILE_FLAGS[@]}" up -d --build postgres keycloak localstack "${APP_SERVICES[@]}"

# ── 2. Wait for REAL health, not just "container started" ─────────────────
# Neither `podman-compose` nor older `docker-compose` supports `--wait`, so poll
# the container health probes the compose file already defines. A container
# without a healthcheck is treated as ready once it is running.
#
# Container names are compose-generated ("wallet_postgres_1") and not
# guaranteed across versions or engines, so resolve the container by its
# compose service label first and fall back to the conventional name.
#
# Both engines are supported because the same script must run locally (podman)
# and in GitHub Actions (docker).
ENGINE=(podman)
[ "$CONTAINER_ENGINE" = "docker" ] && ENGINE=(docker)

resolve_container() {
  local service="$1" name
  name="$("${ENGINE[@]}" ps --filter "label=com.docker.compose.service=$service" --format '{{.Names}}' 2>/dev/null | head -n1)"
  [ -n "$name" ] || name="wallet_${service}_1"
  printf '%s' "$name"
}

wait_healthy() {
  local service="$1" timeout="${2:-180}" waited=0 status name
  name="$(resolve_container "$service")"
  printf '==> waiting for %s (%s)' "$service" "$name"
  while [ "$waited" -lt "$timeout" ]; do
    status="$("${ENGINE[@]}" inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$name" 2>/dev/null || echo missing)"
    case "$status" in
      healthy|running) echo " … $status ($waited s)"; return 0 ;;
      missing)         echo " … container not found" >&2; return 1 ;;
    esac
    printf '.'; sleep 3; waited=$((waited + 3))
  done
  echo " … TIMEOUT after ${timeout}s (last: $status)" >&2
  return 1
}

wait_healthy postgres 120
wait_healthy keycloak 240
wait_healthy localstack 180
for svc in "${APP_SERVICES[@]}"; do wait_healthy "$svc" 180; done

# ── 3. Run the suite against the live stack ────────────────────────────────
# Exported explicitly so `go test` never falls back to a surprising default.
export WALLET_TEST_API_URL="${WALLET_TEST_API_URL:-$API_URL_DEFAULT}"
export WALLET_TEST_DB_DSN="${WALLET_TEST_DB_DSN:-postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable}"
export WALLET_TEST_SQS_ENDPOINT="${WALLET_TEST_SQS_ENDPOINT:-http://localhost:4566}"
export WALLET_TEST_KEYCLOAK="${WALLET_TEST_KEYCLOAK:-http://localhost:8080}"
[ "${MULTI:-0}" = "1" ] && export WALLET_TEST_API_URL_2="${WALLET_TEST_API_URL_2:-http://localhost:8082}" || true

echo "==> running integration suite (race detector on)"
if [ "${UP_ONLY:-0}" = "1" ]; then
  # The stack is up and healthy; the caller (k6, a manual session) owns it now.
  echo "==> UP_ONLY=1: stack is ready, skipping the integration suite"
  echo "    API:${API_URL_DEFAULT}  teardown: $COMPOSE ${PROFILE_FLAGS[*]:-} down -v"
  exit 0
fi

if ! go test -race -count=1 -tags=integration -timeout 15m -v ./test/integration/... "$@"; then
  echo "" >&2
  echo "==> suite FAILED — last 80 lines of the app log (for triage):" >&2
  for svc in "${APP_SERVICES[@]}"; do
    echo "--- $svc ---" >&2
    $COMPOSE ${PROFILE_FLAGS[@]:-} logs --tail=80 "$svc" >&2 || true
  done
  exit 1
fi
