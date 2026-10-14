#!/usr/bin/env bash
# Provision (create + migrate) an ISOLATED test database and print its DSN.
#
# Why this exists: the integration suite (./test/integration/..., and the
# repository-level tests under ./internal/postgres/ and ./internal/transport/http/)
# deliberately exercises paths that leave state the accounting invariants would
# reject if audited. Its own header states it: fresh UUIDv7 ids mean "no
# truncation/teardown is required". That is correct for the tests, but it means
# the suite MUST write to its own database — never the stack's `wallet` DB — or
# `make load-verify` (which audits the whole database) fails on test residue and
# not on a real bug. Pointing the suite at a throwaway database keeps the stack
# database exactly as the service left it, so the integrity audit stays a
# meaningful signal.
#
# Usage:
#   dsn="$(scripts/test-db.sh)"        # prints the DSN on stdout
#   export WALLET_TEST_DB_DSN="$dsn"
#
# Honours WALLET_TEST_DB_DSN (if already set, it is respected and nothing is
# provisioned) and WALLET_TEST_DB_NAME (default: wallet_test). Requires the
# compose Postgres to be up; resolves its container the same way scripts/e2e.sh
# does, so it works under podman and docker alike.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# shellcheck source=scripts/engine.sh
. scripts/engine.sh >/dev/null 2>&1 || true

ENGINE=(podman)
[ "${CONTAINER_ENGINE:-podman}" = "docker" ] && ENGINE=(docker)

if [ -n "${WALLET_TEST_DB_DSN:-}" ]; then
  printf '%s' "$WALLET_TEST_DB_DSN"
  exit 0
fi

TEST_DB_NAME="${WALLET_TEST_DB_NAME:-wallet_test}"

resolve_container() {
  local service="$1" name
  name="$("${ENGINE[@]}" ps --filter "label=com.docker.compose.service=$service" --format '{{.Names}}' 2>/dev/null | head -n1)"
  [ -n "$name" ] || name="wallet_${service}_1"
  printf '%s' "$name"
}

PG_CONTAINER="$(resolve_container postgres)"
if ! "${ENGINE[@]}" ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  echo "test-db: postgres container '$PG_CONTAINER' is not running; start the stack first" >&2
  exit 1
fi

# Drop-and-recreate so every run starts from a pristine schema; a leftover
# database would reintroduce the cross-run contamination this exists to avoid.
"${ENGINE[@]}" exec "$PG_CONTAINER" \
  psql -U wallet -d postgres -v ON_ERROR_STOP=1 -q \
  -c "DROP DATABASE IF EXISTS ${TEST_DB_NAME} WITH (FORCE)" \
  -c "CREATE DATABASE ${TEST_DB_NAME}" >&2

# Migrate the fresh database with the SAME embedded goose migrations the
# service applies at startup, so the test schema cannot drift from prod.
WALLET_DB_DSN="postgres://wallet:wallet@localhost:5433/${TEST_DB_NAME}?sslmode=disable" \
  go run ./cmd/walletd migrate up >&2

printf 'postgres://wallet:wallet@localhost:5433/%s?sslmode=disable' "$TEST_DB_NAME"
