#!/usr/bin/env bash
set -euo pipefail

runtime="${CONTAINER_RUNTIME:-docker}"
name="kbcore-postgres-test-$$"
password="kbcore-test"

cleanup() {
  "$runtime" rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$runtime" run --rm -d \
  --name "$name" \
  -e POSTGRES_PASSWORD="$password" \
  -e POSTGRES_DB=kbcore_test \
  -p 127.0.0.1::5432 \
  pgvector/pgvector:pg16 >/dev/null

for _ in $(seq 1 60); do
  if "$runtime" exec "$name" pg_isready -U postgres -d kbcore_test >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

port="$($runtime port "$name" 5432/tcp | awk -F: 'NR==1 {print $NF}')"
if [[ -z "$port" ]]; then
  echo "failed to discover PostgreSQL test port" >&2
  exit 1
fi

KBCORE_TEST_POSTGRES_DSN="postgres://postgres:${password}@127.0.0.1:${port}/kbcore_test?sslmode=disable" \
  env GOCACHE="${GOCACHE:-/private/tmp/kbcore-gocache}" \
  go test -tags=integration ./internal/postgres -count=1
