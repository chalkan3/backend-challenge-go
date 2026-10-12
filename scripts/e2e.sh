#!/usr/bin/env bash
# End-to-end runner: brings the real stack up, waits for real healthchecks,
# runs the integration suite (including the journey tests), and tears
# everything down — even if a test fails or you hit Ctrl-C.
#
#   ./scripts/e2e.sh              # full stack + suite
#   ./scripts/e2e.sh -run Journey # pass extra flags straight to `go test`
#   KEEP=1 ./scripts/e2e.sh       # leave the stack running afterwards
#
# Works with podman-compose or docker compose; the detection order is
# podman-compose, then docker compose, then docker-compose.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

COMPOSE=""
if command -v podman-compose >/dev/null 2>&1; then
  COMPOSE="podman-compose"
elif docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  echo "error: no compose implementation found (podman-compose or docker compose)" >&2
  exit 1
fi
echo "==> using: $COMPOSE"

PROFILE_FLAGS=()
APP_SERVICES=(app)
if [ "${MULTI:-0}" = "1" ]; then
  PROFILE_FLAGS+=(--profile multi)
  APP_SERVICES+=(app-2 app-3)
fi

cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "==> KEEP=1: leaving the stack up (tear down with: $COMPOSE ${PROFILE_FLAGS[*]:-} down -v)"
    return
  fi
  echo "==> tearing down"
  $COMPOSE "${PROFILE_FLAGS[@]}" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ── 1. Bring the stack up ───────────────────────────────────────────────────
echo "==> bringing the stack up"
$COMPOSE "${PROFILE_FLAGS[@]}" up -d --build postgres keycloak localstack "${APP_SERVICES[@]}"

# ── 2. Wait for REAL health, not just "container started" ──────────────────
# podman-compose has no `--wait`, so poll the container health probes that the
# compose file already defines. A container without a healthcheck is treated as
# ready once it is running.
#
# Container names are compose-generated ("wallet_postgres_1"), which is not
# guaranteed across versions, so resolve the container by its compose service
# label first and fall back to the conventional name.
resolve_container() {
  local service="$1" name
  name="$(podman ps --filter "label=com.docker.compose.service=$service" --format '{{.Names}}' 2>/dev/null | head -n1)"
  [ -n "$name" ] || name="wallet_${service}_1"
  printf '%s' "$name"
}

wait_healthy() {
  local service="$1" timeout="${2:-180}" waited=0 status name
  name="$(resolve_container "$service")"
  printf '==> waiting for %s (%s)' "$service" "$name"
  while [ "$waited" -lt "$timeout" ]; do
    status="$(podman inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$name" 2>/dev/null || echo missing)"
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
export WALLET_TEST_API_URL="${WALLET_TEST_API_URL:-http://localhost:8081}"
export WALLET_TEST_DB_DSN="${WALLET_TEST_DB_DSN:-postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable}"
export WALLET_TEST_SQS_ENDPOINT="${WALLET_TEST_SQS_ENDPOINT:-http://localhost:4566}"
export WALLET_TEST_KEYCLOAK="${WALLET_TEST_KEYCLOAK:-http://localhost:8080}"
[ "${MULTI:-0}" = "1" ] && export WALLET_TEST_API_URL_2="${WALLET_TEST_API_URL_2:-http://localhost:8082}" || true

echo "==> running integration suite (race detector on)"
if ! go test -race -count=1 -tags=integration -timeout 15m -v ./test/integration/... "$@"; then
  echo "" >&2
  echo "==> suite FAILED — last 80 lines of the app log (for triage):" >&2
  $COMPOSE logs --tail=80 app >&2 || true
  exit 1
fi
