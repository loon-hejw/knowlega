# Knowlega — QM Knowledge Agent

Knowlega is the Go backend of QM. The LLM Wiki is no longer a standalone
service: it is an internal Agent that turns QM personal memory, project files,
and valuable completed conversations into a durable, evidence-grounded wiki.
QM remains the product boundary and user-facing UI; Markdown is the knowledge
source of truth and PostgreSQL is its derived search/graph/operational index.

The design draws from three reference systems:

- **LLM Wiki** for analysis-first ingest, durable wiki synthesis, citations,
  and maintenance.
- **GitNexus** for exact code-symbol, call, route, impact, and process graphs.
- **graphify** for portable graph snapshots and readable graph reports.

Knowledge Core is deliberately not ordinary RAG. QM's outer Pi loop is the
only user-facing reasoning agent. It uses one deterministic `knowledge` tool
to navigate, read durable wiki/raw/graph evidence, validate a submission, and
explicitly write a useful synthesis when requested.

## Documentation

- [Architecture](docs/architecture.md): source-of-truth rules, ingest, query,
  graph, and PostgreSQL boundaries.
- [QM integration direction](docs/adr/0001-qm-platform-knowledge-core-plugin.md):
  QM as the product host and Knowlega as its internal Agent.
- [Configuration](docs/configuration.md): every QM backend YAML section,
  provider setup, paths, security, and precedence.
- [Service API](docs/service-api.md): QM backend lifecycle, authentication, and
  route groups.
- [Development and validation](docs/development.md): builds, tests, acceptance
  scripts, and repository conventions.
- [Agent instructions](AGENTS.md): mandatory implementation direction and
  maintenance constraints for coding agents.

## Quick Start

Prerequisites: Go 1.25+, Node.js/npm, Docker (or Colima), and an
OpenAI-compatible or Anthropic-compatible model for QM plus semantic ingest and
maintenance. The development supervisor provisions the local pgvector
PostgreSQL container automatically. Mock providers are local plumbing fixtures
only.

```bash
cp configs/qm-config.example.yaml configs/qm-config.yaml
$EDITOR configs/qm-config.yaml

cd qm
npm run dev-instance:no-slack -- --force
```

This starts and verifies the complete browser stack: PostgreSQL, `qm-backend`,
the QM runtime, and the unified user/admin frontend. Open
`http://localhost:8129/` when the command reports success. Runtime settings
come from the QM YAML generated for the development instance; local configs and
credentials remain ignored and must not be committed.

```bash
npm run dev-instance:status   # inspect child health and ports
npm run dev-instance:doctor   # diagnose local prerequisites
npm run dev-instance:down     # stop the instance
```

After changing `configs/qm-config.yaml`, run
`npm run dev-instance:no-slack -- --force` again to reload the YAML and restart
the affected children. Configuration is loaded at process startup and is not
hot-reloaded by `npm run dev`.

In the IDE's Run and Debug menu, start the two services independently:

- **Go 后端**: runs `cmd/qm-backend --config config.yaml` under the Go debugger,
  after the required `local-pg guard knowledge-core` check.
- **前端**: runs `npm start` in `plugins/web-ui` with Node 24, which builds and
  serves the frontend at `http://localhost:18129/`. It connects to the backend
  at `http://127.0.0.1:19829/` without waiting for it to become ready.

Both services show their output in the IDE's integrated Terminal panel.
The backend reads the root `config.yaml` in QM format. Frontend authentication
settings are loaded from the ignored `config.local/ide-frontend.env`.
Use `local-pg env knowledge-core --target host` for database connection settings.

## Run the Local Application

The backend owns HTTP and QM gRPC control/runner services. Knowlega has no
public Knowledge gRPC service or standalone server; it is invoked internally
after QM writes succeed:

```bash
env GOCACHE=/private/tmp/knowlega-gocache \
  go run ./cmd/qm-backend --config configs/qm-config.yaml
```

The service is consumed through QM's HTTP API and control/runner gRPC. There is
no separate Knowledge panel or browser-side persistence implementation.

## Core Artifact Layout

```text
project/
  purpose.md
  schema.md
  raw/
    sources/<collection>/<slug>-<hash12>/
      metadata.json
      original/<source-file>
      extracted.md                  # extracted text when needed
    code-graphs/
  wiki/
    index.md
    overview.md
    log.md
    reviews.md
    sources/
    concepts/
    entities/
    code/
    syntheses/
  .kbcore/
    source-manifest.json
    ingest-queue.json
    page-versions/
    graph-snapshots/
    graph-jobs.json
    relations.json
```

`raw/` is immutable evidence. `wiki/` is the durable knowledge product.
`.kbcore/` contains recoverable manifests, queues, versions, and generated
graph state. PostgreSQL can be rebuilt from those artifacts.

## Build and Test

```bash
env GOCACHE=/private/tmp/knowlega-gocache go test ./...
```

The internal Agent acceptance coverage lives in
`internal/agent/knowlega/agent_test.go` and the package-level LLM Wiki tests.
See [Development and validation](docs/development.md) for the expected
evidence and test scope.
