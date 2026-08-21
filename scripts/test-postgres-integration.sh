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
  --label io.local-pg.scope=ephemeral-test \
  -e POSTGRES_PASSWORD="$password" \
  -e POSTGRES_DB=knowlega_test \
  -p 127.0.0.1::5432 \
  pgvector/pgvector:pg16 >/dev/null

database_ready=false
for _ in $(seq 1 60); do
  if "$runtime" exec -e "PGPASSWORD=$password" "$name" \
    psql -U postgres -d knowlega_test -Atqc 'SELECT 1' >/dev/null 2>&1; then
    database_ready=true
    break
  fi
  sleep 1
done
if [[ "$database_ready" != true ]]; then
  echo "PostgreSQL test database did not become ready" >&2
  exit 1
fi

port="$($runtime port "$name" 5432/tcp | awk -F: 'NR==1 {print $NF}')"
if [[ -z "$port" ]]; then
  echo "failed to discover PostgreSQL test port" >&2
  exit 1
fi

"$runtime" exec -e "PGPASSWORD=$password" "$name" \
  psql -U postgres -d knowlega_test -c 'CREATE SCHEMA IF NOT EXISTS knowledge_core' >/dev/null

KBCORE_TEST_POSTGRES_DSN="postgres://postgres:${password}@127.0.0.1:${port}/knowlega_test?sslmode=disable&search_path=knowledge_core%2Cpublic" \
QM_BACKEND_TEST_DATABASE_URL="postgres://postgres:${password}@127.0.0.1:${port}/knowlega_test?sslmode=disable" \
  env GOCACHE="${GOCACHE:-/private/tmp/knowlega-gocache}" \
  go test -p=1 -timeout="${GO_TEST_TIMEOUT:-40m}" -tags=integration ./internal/agent/knowlega/postgres ./internal/qm/data ./internal/qm/agent ./internal/qm/server ./cmd/qm-backend -count=1
