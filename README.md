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

Knowledge Core is deliberately not ordinary RAG: search retrieves candidates,
but an LLM planner navigates and reads durable wiki/raw evidence before it can
answer or write a synthesis back to the wiki.

## Documentation

- [Architecture](docs/architecture.md): source-of-truth rules, ingest, query,
  graph, and PostgreSQL boundaries.
- [QM integration direction](docs/adr/0001-qm-platform-knowledge-core-plugin.md):
  QM as the product host and Knowlega as its internal Agent.
- [Configuration](docs/configuration.md): every `config.yaml` section,
  provider setup, paths, security, and precedence.
- [Service API](docs/service-api.md): QM backend lifecycle, authentication, and
  route groups.
- [Development and validation](docs/development.md): builds, tests, acceptance
  scripts, and repository conventions.
- [Agent instructions](AGENTS.md): mandatory implementation direction and
  maintenance constraints for coding agents.

## Quick Start

Prerequisites: Go 1.25+, PostgreSQL with pgvector, and an OpenAI-compatible or
Anthropic-compatible LLM for semantic ingest/query work. The Agent falls back
to deterministic mock behavior for local plumbing tests only.

```bash
cp configs/qm-config.example.yaml qm-backend/configs/config.yaml
$EDITOR qm-backend/configs/config.yaml

env GOCACHE=/private/tmp/knowlega-gocache \
  go run ./cmd/qm-backend --config qm-backend/configs/config.yaml
```

Runtime settings come from the QM YAML file passed to `cmd/qm-backend`; local
configs and credentials remain ignored and must not be committed.

## Run the Local Application

The backend owns HTTP and QM gRPC control/runner services. Knowlega has no
public Knowledge gRPC service or standalone server; it is invoked internally
after QM writes succeed:

```bash
go run ./cmd/qm-backend --config qm-backend/configs/config.yaml
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
