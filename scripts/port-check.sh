#!/usr/bin/env bash
# Pre-flight port check for the compose stack.
#
# The compose file publishes fixed host ports (8080 keycloak, 8081 app, 4566
# localstack, ...). When something else already owns one of them — most commonly
# a `kind` cluster whose NodePorts map onto the very same numbers — `compose up`
# creates the containers but they never start, and the failure is silent: the
# service sits in "Created" with no obvious error. This script turns that into a
# fast, explicit failure that names the offender.
#
#   ./scripts/port-check.sh              # check default ports
#   PROFILES="multi" ./scripts/port-check.sh
#
# Exits non-zero (and prints what to kill) if any required port is taken.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# shellcheck source=scripts/engine.sh
. scripts/engine.sh

# host port -> human label, for the services we are about to start.
PORTS=(
  "5433:postgres"
  "8080:keycloak"
  "4566:localstack"
)
case "${PROFILES:-}" in
  *multi*) PORTS+=( "8082:app-2" "8083:app-3" ) ;;
  *split*) PORTS+=( "8084:api" "8085:workers" ) ;;
  *)       PORTS+=( "8081:app" ) ;;
esac

listener_for() {
  local port="$1" out
  # ss is present on Linux; fall back to lsof, then to /dev/tcp probe.
  if command -v ss >/dev/null 2>&1; then
    out="$(ss -ltnp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$" {print $0}' | head -n1)"
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    return 1
  fi
  if command -v lsof >/dev/null 2>&1; then
    out="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR==2{print $1" (pid "$2")"}')"
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    return 1
  fi
  (exec 3<>"/dev/tcp/127.0.0.1/$port") >/dev/null 2>&1 && { printf 'listening'; return 0; }
  return 1
}

# A port published by a container of *this* compose project is not a conflict:
# it just means the stack is already up, which `up -d` handles idempotently. We
# detect that by looking for a running container with the project's compose
# label whose published port matches — via `$COMPOSE ps` would need port
# introspection we can't do portably, so we fall back to the engine's inspect
# and match on the host-side port.
port_owned_by_project() {
  local port="$1" engine="${CONTAINER_ENGINE:-}" name ports
  [ -z "$engine" ] && { . ./scripts/engine.sh >/dev/null 2>&1 || return 1; engine="$CONTAINER_ENGINE"; }
  command -v "$engine" >/dev/null 2>&1 || return 1
  # Iterate with a here-string (no subshell) so `return 0` propagates.
  while read -r name; do
    [ -n "$name" ] || continue
    ports="$("$engine" inspect "$name" --format '{{json .NetworkSettings.Ports}}' 2>/dev/null || true)"
    case "$ports" in
      *"\"HostPort\":\"$port\""*) return 0 ;;
    esac
  done <<EOF
$("$engine" ps --filter "label=com.docker.compose.project=wallet" --format '{{.Names}}' 2>/dev/null || true)
EOF
  return 1
}

conflicts=()
for entry in "${PORTS[@]}"; do
  port="${entry%%:*}"; label="${entry#*:}"
  if who="$(listener_for "$port")"; then
    # Our own stack already holding the port is fine (idempotent `up -d`).
    if port_owned_by_project "$port"; then
      continue
    fi
    conflicts+=("  port $port ($label) already in use by: $who")
  fi
done

if [ "${#conflicts[@]}" -gt 0 ]; then
  echo "error: host ports needed by the compose stack are taken:" >&2
  printf '%s\n' "${conflicts[@]}" >&2
  echo "" >&2
  echo "hint: if the owner is a kind cluster (look for 'wallet-control-plane')," >&2
  echo "      run 'make kind-down' (or 'kind delete cluster --name wallet') first." >&2
  echo "      kind and compose publish the same ports and cannot coexist." >&2
  exit 1
fi

echo "==> port pre-flight OK (${#PORTS[@]} ports free)"
