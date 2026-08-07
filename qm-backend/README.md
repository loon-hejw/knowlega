# QM Backend

Kratos control plane for QM. It preserves the existing `/v1` HTTP contract while
moving durable control-plane state from the Node core to Go. Node remains the
agent runner and surface adapter until those boundaries are migrated.

Run locally with a PostgreSQL `DATABASE_URL`:

```bash
go run ./cmd/qm-backend --config configs/config.yaml
```

Generate the checked-in gRPC bindings with `make generate`; run the normal suite
with `make test`. PostgreSQL integration tests are enabled by setting
`QM_BACKEND_TEST_DATABASE_URL`.

See [Node Core Migration](docs/node-core-migration.md) for the deployment and
route-cutover procedure.
