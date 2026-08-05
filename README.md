# Knowledge Core

Knowledge Core is a Go/PostgreSQL/Markdown implementation of a persistent,
LLM-maintained knowledge base. It compiles immutable source material into an
evolving Markdown wiki, uses PostgreSQL as rebuildable index and graph state,
and keeps the resulting knowledge readable by humans, agents, Git, and
Obsidian.

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
  QM as the collaboration platform and Knowledge Core as the project/personal
  knowledge plugin.
- [QM integration contract](docs/qm-integration.md): experimental gRPC
  boundary, scope mapping, and the first read-only tool surface.
- [Configuration](docs/configuration.md): every `config.yaml` section,
  provider setup, paths, security, and precedence.
- [CLI reference](docs/cli.md): command groups and common workflows.
- [Service API](docs/service-api.md): server lifecycle, authentication, route
  groups, Web UI, and MCP.
- [Development and validation](docs/development.md): builds, tests, acceptance
  scripts, and repository conventions.
- [Agent instructions](AGENTS.md): mandatory implementation direction and
  maintenance constraints for coding agents.

## Quick Start

Prerequisites: Go 1.22+, and an OpenAI-compatible or Anthropic-compatible LLM
for real ingest/query work. PostgreSQL with pgvector is optional.

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml init \
  --path /private/tmp/kbcore-demo --name demo

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml validate-llmwiki \
  --project /private/tmp/kbcore-demo \
  --source ./README.md --agent llm

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore --config config.yaml query \
  --project /private/tmp/kbcore-demo \
  --q "What does this project build?" --agent llm
```

Runtime settings come only from repository-root `config.yaml`, or the path
passed with global `--config`. CLI flags override YAML; YAML overrides program
defaults. The runtime intentionally ignores legacy environment configuration.
Do not commit `config.yaml` or real credentials.

## Run the Local Application

The backend can initialize/bootstrap its configured project while serving
readiness and progress:

```bash
go run ./cmd/kbcore --config config.yaml serve
```

In another terminal:

```bash
cd web
npm install
npm run dev
```

The Vite app uses the backend API and opens the configured active project. For
PostgreSQL/pgvector setup, authentication, workers, graph webhooks, and
production behavior, see [Service API](docs/service-api.md) and
[Configuration](docs/configuration.md).

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
env GOCACHE=/private/tmp/kbcore-gocache go test ./...

cd web
npm test -- --run
npm run build
```

Use `scripts/verify-xiyouji.sh` for the deterministic 100-source accumulation
test. Use `scripts/verify-llmwiki-llm.sh config.yaml` for the small real-LLM
acceptance path. See [Development and validation](docs/development.md) for the
expected evidence and test scope.
