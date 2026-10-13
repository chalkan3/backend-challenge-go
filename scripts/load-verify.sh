#!/usr/bin/env bash
# Independent integrity audit, run against the database directly.
#
# This is the half of the load test that k6 cannot do: k6 proves the API
# answered correctly, this proves what was committed is internally consistent.
# The two together are the actual claim; neither alone is enough.
#
# Every audit in loadtest/sql/invariants.sql prints
#   AUDIT|<n>|<description>|<value>
# and passes only when <value> is 0. An INFO|<n>|<description>|<value> line is
# reported but never fails the run.
set -euo pipefail

cd "$(dirname "$0")/.."

PG_CONTAINER="${PG_CONTAINER:-wallet_postgres_1}"
PG_USER="${PG_USER:-wallet}"
PG_DB="${PG_DB:-wallet}"

if ! command -v docker >/dev/null 2>&1; then
  echo "load-verify: docker/podman not found on PATH" >&2
  exit 1
fi

if ! docker ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  echo "load-verify: container '$PG_CONTAINER' is not running." >&2
  echo "             start the stack with 'make up' (or set PG_CONTAINER=...)." >&2
  exit 1
fi

echo "load-verify: auditing $PG_CONTAINER ($PG_DB)"

out="$(docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -f - < loadtest/sql/invariants.sql)"

audits=0
failures=0
while IFS= read -r line; do
  case "$line" in
    AUDIT\|*)
      IFS='|' read -r _ num desc value <<< "$line"
      audits=$((audits + 1))
      if [[ "$value" == "0" ]]; then
        printf '  ok    %-52s %s\n' "$desc" "$value"
      else
        printf '  FAIL  %-52s %s\n' "$desc" "$value"
        failures=$((failures + 1))
      fi
      ;;
    INFO\|*)
      IFS='|' read -r _ num desc value <<< "$line"
      printf '  info  %-52s %s\n' "$desc" "$value"
      ;;
    *ERROR*|*FATAL*)
      echo "  !! $line" >&2
      failures=$((failures + 1))
      ;;
  esac
done <<< "$out"

echo
if [[ "$audits" -eq 0 ]]; then
  echo "load-verify: FAILED — no audits ran (is the schema migrated?)" >&2
  exit 1
fi
if [[ "$failures" -gt 0 ]]; then
  echo "load-verify: FAILED — $failures of $audits audit(s) found a violation"
  exit 1
fi

echo "load-verify: OK — $audits integrity audits, no violations"
