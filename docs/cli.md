# CLI Reference

Use the global configuration flag before the command:

```bash
go run ./cmd/kbcore --config config.yaml <command> [flags]
```

Run `kbcore help` for the exact flag synopsis of the current binary. Project
and database flags can override configured defaults for one invocation.

## Project and Source Commands

| Command | Purpose |
| --- | --- |
| `init` | Create `purpose.md`, `schema.md`, raw/wiki directories, and navigation artifacts. |
| `ingest` | Archive one immutable source and create/update its deterministic source page and manifest entry. |
| `validate-llmwiki` | Run analysis-first LLM compilation for a file or directory. |
| `queue-ingest` | Add a source to the recoverable ingest queue. |
| `scan-sources` | Scan the source area and enqueue work. |
| `run-queue` | Consume queued ingest work with retry and manifest handling. |
| `maintain` | Run the combined queue/review/sync maintenance workflow. |
| `source-layout migrate` | Preview or apply migration from legacy flat sources to per-source archives. |
| `source-layout status` | Inspect source-layout migration state. |

Typical flow:

```bash
kbcore --config config.yaml init --path /tmp/demo-kb --name demo
kbcore --config config.yaml validate-llmwiki \
  --project /tmp/demo-kb --source ./documents --agent llm --skip-unchanged
kbcore --config config.yaml lint --project /tmp/demo-kb
```

For repeated imports:

```bash
kbcore --config config.yaml queue-ingest \
  --project /tmp/demo-kb --source ./documents/new.md
kbcore --config config.yaml run-queue \
  --project /tmp/demo-kb --agent llm --retry-failed
```

## Query and Maintenance Commands

| Command | Purpose |
| --- | --- |
| `query` | Run the planner-driven evidence action loop and return cited synthesis. |
| `lint` | Run deterministic structural lint; `--agent llm` adds semantic review. |
| `review-wiki` | Run the semantic wiki maintenance pass. |
| `review-tasks` | List durable entries from `wiki/reviews.md`. |
| `resolve-review` | Change a review item status in the Markdown artifact. |

```bash
kbcore --config config.yaml query \
  --project /tmp/demo-kb --q "How are sources archived?" --agent llm

kbcore --config config.yaml query \
  --project /tmp/demo-kb --q "Summarize the archive design" \
  --agent llm --save-title auto

kbcore --config config.yaml review-wiki \
  --project /tmp/demo-kb --agent llm
```

Offline fallback and mock agents are deterministic test scaffolds. They cannot
authorize synthesis writeback and should not be treated as semantic reasoning.

## PostgreSQL Commands

| Command | Purpose |
| --- | --- |
| `migrate-sql` | Print the PostgreSQL bootstrap SQL. It does not require runtime config. |
| `sync-wiki-pg` | Fully rebuild derived wiki/review/manifest/version state; optionally embed pages. |

```bash
kbcore migrate-sql > /tmp/kbcore-schema.sql
kbcore --config config.yaml sync-wiki-pg \
  --project /tmp/demo-kb --migrate-db --embed
```

Full sync prunes PostgreSQL rows absent from durable Markdown/manifest state.
Normal ingest, LLM compilation, graph import, and query writeback use
incremental sync and do not prune unrelated rows.

## Code Graph Commands

| Command | Purpose |
| --- | --- |
| `code-index-go` | Build exact Go package/symbol/call/route/tool/community/process facts. |
| `code-import-graphify` | Import graphify/GitNexus-style JSON and an optional Markdown report. |

```bash
kbcore --config config.yaml code-index-go \
  --project /tmp/demo-kb --repo-path . --repo-id knowledge-core \
  --include-tests --migrate-db

kbcore --config config.yaml code-import-graphify \
  --project /tmp/demo-kb --repo-path /path/to/repo \
  --repo-id example --graph /path/to/graph.json \
  --report /path/to/GRAPH_REPORT.md
```

## Runtime Commands

| Command | Purpose |
| --- | --- |
| `serve` | Start the HTTP service and optional ingest/graph workers. |
| `wait-ready` | Poll the configured service until bootstrap is complete. |
| `mcp` | Start the stdio MCP server for agent clients. |

```bash
kbcore --config config.yaml serve --migrate-db
kbcore --config config.yaml wait-ready --timeout 2h
kbcore --config config.yaml mcp --project /tmp/demo-kb
```

