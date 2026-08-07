#!/usr/bin/env bash
set -euo pipefail

runtime="${CONTAINER_RUNTIME:-docker}"
name="knowlega-postgres-test-$$"
password="knowlega-test"

cleanup() {
  "$runtime" rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$runtime" run --rm -d \
  --name "$name" \
  -e POSTGRES_PASSWORD="$password" \
  -e POSTGRES_DB=knowlega_test \
  -p 127.0.0.1::5432 \
  pgvector/pgvector:pg16 >/dev/null

for _ in $(seq 1 60); do
  if "$runtime" exec "$name" pg_isready -U postgres -d knowlega_test >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

port="$($runtime port "$name" 5432/tcp | awk -F: 'NR==1 {print $NF}')"
if [[ -z "$port" ]]; then
  echo "failed to discover PostgreSQL test port" >&2
  exit 1
fi

KBCORE_TEST_POSTGRES_DSN="postgres://postgres:${password}@127.0.0.1:${port}/knowlega_test?sslmode=disable" \
QM_BACKEND_TEST_DATABASE_URL="postgres://postgres:${password}@127.0.0.1:${port}/knowlega_test?sslmode=disable" \
  env GOCACHE="${GOCACHE:-/private/tmp/knowlega-gocache}" \
  go test -tags=integration ./internal/agent/knowlega/postgres ./internal/qm/data -count=1
