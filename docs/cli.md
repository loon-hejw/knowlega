# CLI reference

There is no standalone `kbcore` CLI in the QM-hosted architecture. Knowlega is
an internal Agent and is invoked by the Go QM backend after successful file,
memory, and completed-conversation writes.

For local development, start the backend with:

```bash
env GOCACHE=/private/tmp/knowlega-gocache \
  go run ./cmd/qm-backend --config configs/qm-config.yaml
```

Agent-level behavior is covered by Go tests in
`internal/agent/knowlega/{compiler,service,wiki}` and the integration facade in
`internal/agent/knowlega/agent_test.go`. Use the HTTP API or QM control/runner
gRPC for application integration; do not add a second Knowledge transport.
