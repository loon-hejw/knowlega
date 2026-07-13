# Knowledge Core

Go + PostgreSQL + Markdown knowledge-base core inspired by:

- `llm_wiki`: persistent LLM-maintained wiki workflow.
- `GitNexus`: code semantic graph model.
- `graphify`: graph snapshot/report/wiki export ideas.

This module deliberately keeps the first implementation free of Node runtime
dependencies. Markdown files are the durable, human-readable artifact; PostgreSQL
is the indexed state, graph, task, review, and retrieval layer.
`wiki/overview.md` is maintained as the compact synthesis/navigation entrypoint
for LLM ingest and query workflows.
Generated pages can use `aliases` frontmatter for common names and user-facing
wording; candidate recall reads that metadata without hard-coded corpus rules.
`wiki/index.md` is maintained by content sections such as Sources, Concepts,
Entities, Syntheses, and Code.
When generated wiki pages are overwritten, the previous page is archived under
`.kbcore/page-versions/` before the new content is written.

## Current Scope

- `kbcore init`: create a local wiki project layout.
- `kbcore ingest`: archive an immutable source under its own
  `raw/sources/<collection>/<slug>-<hash12>/` directory and compile a
  source summary page into `wiki/sources/`. It also records the raw source and
  generated source page in `.kbcore/source-manifest.json`.
- `kbcore source-layout migrate`: preview the conversion of legacy flat
  `raw/sources/` files into per-source archives. Add `--apply` to execute the
  journaled/resumable migration. It updates Markdown provenance, manifest,
  queue, and watch state without invoking an LLM; `source-layout status` reads
  `.kbcore/source-layout-migration.json`.
- `kbcore validate-llmwiki`: run the LLM Wiki ingest flow for one file or a
  directory of `.txt`/`.md` files, generating source/concept/entity/synthesis
  pages through strict `---FILE:`/`---REVIEW:` blocks. The user-facing runtime
  requires `--agent llm` with an OpenAI-compatible or Anthropic Messages
  provider.
  Review blocks are persisted in `wiki/reviews.md`. Pass `--skip-unchanged` to
  avoid reprocessing sources whose SHA256 is unchanged in
  `.kbcore/source-manifest.json`.
- `kbcore queue-ingest`, `kbcore scan-sources`, and `kbcore run-queue`: maintain
  `.kbcore/ingest-queue.json` as a durable ingest queue. Queue execution
  serializes LLM work, records task status/retry metadata, reuses the
  `validate-llmwiki` compiler, and can skip unchanged sources through the source
  manifest.
- `kbcore query`: run the LLM Wiki query workflow: read the navigation files,
  produce a query plan, use local search only as candidate recall, then return
  an answer/citation bundle. Query uses the LLM action loop; pass `--save-title`
  to write a useful LLM-backed answer back into `wiki/syntheses/`.
- `kbcore lint`: deterministic checks for broken links, orphan pages, missing
  outlinks, and known title/alias mentions that should be `[[wikilink]]`
  cross-references. Pass `--agent llm` to run the structural checks and then the
  same LLM semantic review used by `review-wiki`.
- `kbcore review-wiki`: LLM-backed semantic wiki health review for
  contradictions, duplicate pages, missing pages, stale claims, source gaps, and
  review follow-up. This is the LLM Wiki maintenance pass; deterministic
  `lint` remains the structural guardrail. The review prompt includes
  `purpose.md`, `schema.md`, `wiki/index.md`, `wiki/overview.md`, wiki page
  excerpts, `.kbcore/source-manifest.json`, and raw source excerpts.
- `kbcore review-tasks` and `kbcore resolve-review`: list and update durable
  review items in `wiki/reviews.md`. Review status remains Markdown-first and is
  synced into PostgreSQL by the existing wiki sync path.
- `kbcore code-import-graphify`: import a graphify `graph.json`/report snapshot
  and compile a code overview page.
- `kbcore code-index-go`: build an exact Go-native semantic graph from a local
  repository. It records packages, files, functions, methods, types,
  interfaces, calls, imports, implementations, routes, tools, deterministic
  communities, and bounded process flows under `.kbcore/graph-snapshots/`, then
  creates versioned code overview/community/process Wiki pages. Every snapshot
  records the Git commit, dirty-worktree state, and SHA256 of the indexed Go
  sources so local changes cannot masquerade as the clean commit.
- `kbcore serve`: run the same knowledge core as a local HTTP JSON service.
  The service exposes project init, ingest queue, LLM Wiki validate, query
  writeback, review tasks, wiki review, PG sync, lint, and code graph import
  endpoints. The graph API supports filtered, cursor-bounded subgraphs and
  node expansion instead of returning the entire graph. With `graph.enabled`,
  configured GitHub/GitLab/Gitea push webhooks enqueue allowlisted repository
  indexing; `graph.worker` consumes the restart-safe graph queue. Pass
  `--project` for a default wiki project and `--worker` to
  periodically scan `raw/sources/` and consume the ingest queue.
- `kbcore migrate-sql`: print the PostgreSQL bootstrap schema.

The PostgreSQL repository layer is designed around `database/sql` so the driver
choice can remain outside the core. A production binary can wire `pgx` or any
other driver without changing the domain model. PostgreSQL is derived state for
projects, sources, source manifests, wiki pages, page versions, review items,
query logs, graph facts, FTS, and pgvector embeddings; Markdown remains the
durable wiki artifact. Use `wiki.ScanWikiPages` to rebuild structured wiki page
records from Markdown, then `postgres.Store.UpsertWikiPages` to sync those pages
into PostgreSQL. Full wiki sync is replacement-oriented for `wiki_pages`: rows
whose paths no longer exist in Markdown are pruned, while incremental path sync
only updates the pages written by the current operation. Full wiki sync also
imports archived Markdown versions from `.kbcore/page-versions/` into
`wiki_page_versions` and prunes stale PG version rows so page evolution remains
queryable from PostgreSQL without making PG a second source of truth.
Review entries persisted in `wiki/reviews.md` are synced into `review_items`;
full sync prunes PG review rows no longer present in Markdown, while incremental
sync only upserts current review entries. `.kbcore/source-manifest.json` is also
synced into `source_manifest`; full sync prunes stale manifest rows, while
post-write incremental sync upserts the current manifest without deleting other
rows. The same manifest drives PG `sources` rows for immutable raw source
provenance, including raw path, original path, title, SHA256, and import time. Use
`service.SyncCodeGraphSnapshot` to sync imported GitNexus/graphify snapshots
into `code_repos`, `graph_nodes`, and `graph_edges` with project/repo-scoped
stable IDs. When backed by `postgres.Store`, code graph snapshot sync runs repo,
node, and edge writes in one transaction and replaces the previous graph facts
for that repo.

## Query Model

`kbcore query` is not intended to be a plain search command. In the target LLM
Wiki workflow, the LLM reads `wiki/index.md`, `wiki/overview.md`, and
`wiki/log.md`, decides which pages to inspect, optionally calls lexical/FTS/
vector/graph search as candidate recall, reads the returned pages or raw
sources, and synthesizes an answer with citations. The query plan's `read_first`
pages are passed into synthesis even when no search is requested.

LLM-backed query agents can run a bounded action loop with `read`,
`list_pages`, `follow_links`, `search`, `graph`, `final`, and `writeback`
actions.
`list_pages` exposes wiki navigation without making factual claims, and
`follow_links` reads linked wiki pages so the agent can traverse the wiki graph
before broad recall. `read` accepts safe project-relative paths plus exact wiki
titles, `[[wikilink]]` names, and aliases, then resolves them back to Markdown
pages. Navigation observations are passed into the next LLM action separately
from evidence documents. `search` returns candidates; it does not make claims.
After `search` reads candidate wiki pages, the runtime expands through the
ordinary wiki graph using direct links, source overlap, common neighbors, and
type affinity. The `graph` action combines imported code graph evidence with
ordinary wiki graph evidence, so conceptual pages can support non-code
questions without turning search snippets into answers.
With PostgreSQL configured, `search` can use pgvector embeddings when an
embedding provider is supplied, then falls back to
`wiki_pages.search_vector`; `graph` prefers `code_repos`, `graph_nodes`, and
`graph_edges`. When no store/evidence is available, they fall back to
Markdown/raw files under `wiki/`, `raw/sources/`, and `raw/code-graphs/`. After a
`search` action, the runtime auto-reads the top candidate wiki/raw files into
the synthesis context. A `final` or `writeback` action without a read wiki/raw
page or graph evidence is rejected, so search snippets and page lists cannot
become the answer by themselves. `writeback` records the LLM's suggested
synthesis title; it does not write files unless the caller passes `--save-title`
or `save_title`. Query citations are built from read evidence documents, not raw
search results. The CLI prints the action trace and supports `--save-title auto`
to use the LLM-suggested title.
The offline fallback keeps tests deterministic, but it is only a scaffold. Use
`--agent llm` for a real LLM-driven planner/action/synthesis workflow. CLI/API
database configuration is read from `database.dsn` and `database.project_id` in
`config.yaml`, with explicit `--db-dsn`/`--project-id` CLI overrides. Query
writeback is enforced in the service layer: only non-offline answers with
`can_write_back=true` and at least one non-navigation citation can be saved to
`wiki/syntheses/`. When PostgreSQL is configured, completed queries are also
recorded in `query_logs` so recurring questions, missing concepts, and useful
synthesis seeds remain available for later wiki maintenance.

`postgres.Store.UpsertWikiPageEmbedding` writes page embeddings into
`wiki_pages.embedding`; `SearchWikiEvidenceVector` uses pgvector distance for
candidate recall. Embedding generation is intentionally an injected provider so
the core does not pretend deterministic tests are semantic embeddings.
PostgreSQL FTS and fallback recall include `aliases` frontmatter as first-class
semantic naming metadata, matching the file-search scorer's alias boost.
Page embedding text includes title, type, aliases, sources, and body, so changes
to semantic naming or provenance refresh embeddings through the stored source
hash.
Use `sync-wiki-pg --embed` with the `embedding` section in `config.yaml` to
populate page embeddings. The runtime retries `/v1/embeddings` if a root
`/embeddings` request returns 404, and `embedding.max_input_chars` bounds long
page text before embedding so smaller models do not reject large wiki pages.
When `ingest`, `validate-llmwiki`, `query --save-title`, or
`code-import-graphify` run with `--db-dsn`/`--project-id`, the pages they write
are incrementally synced into PG; if an embedding model is configured, those
page embeddings are refreshed only when the embedding text hash or embedding
model changes. PostgreSQL stores `embedding_model`,
`embedding_source_sha256`, and `embedding_updated_at` beside the pgvector value.
The API server follows the same write-after-sync rule when started with a
PostgreSQL store and project id, including graphify code imports.
When the backing store supports transactions, wiki page sync runs page and
embedding metadata updates in one store transaction.
Code graph sync also uses a store transaction when available, so repo/node/edge
facts do not partially commit.

## Project Layout

```text
project/
  purpose.md
  schema.md
  raw/
    sources/
      <collection>/
        <slug>-<sha256-prefix>/
          metadata.json
          original/
            <original-file>
          extracted.md        # PDF/DOCX only
    code-graphs/
  wiki/
    index.md
    log.md
    overview.md
    reviews.md
    sources/
    code/
    syntheses/
  .kbcore/
    graph-snapshots/
    graph-jobs.json
    relations.json            # generated; never a hand-edited truth source
```

## Build And Test

```bash
go test ./...
bash scripts/verify-xiyouji.sh
go run ./cmd/kbcore --config config.yaml init --path /tmp/demo-kb --name demo
go run ./cmd/kbcore --config config.yaml ingest --project /tmp/demo-kb --source ./README.md
go run ./cmd/kbcore --config config.yaml validate-llmwiki --project /tmp/demo-kb --source ./tst/xiyouji-chapters --agent llm --skip-unchanged
go run ./cmd/kbcore --config config.yaml queue-ingest --project /tmp/demo-kb --source ./tst/xiyouji-chapters/chapter-054.txt
go run ./cmd/kbcore --config config.yaml run-queue --project /tmp/demo-kb --agent llm
go run ./cmd/kbcore --config config.yaml source-layout migrate --project /tmp/demo-kb
go run ./cmd/kbcore --config config.yaml source-layout migrate --project /tmp/demo-kb --apply --migrate-db
go run ./cmd/kbcore --config config.yaml code-index-go --project /tmp/demo-kb --repo-path . --repo-id knowledge-core --migrate-db
go run ./cmd/kbcore --config config.yaml query --project /tmp/demo-kb --q graph
go run ./cmd/kbcore --config config.yaml lint --project /tmp/demo-kb
bash scripts/verify-llmwiki-llm.sh
```

## Local Docker Test Config

Start a local PostgreSQL database with pgvector:

```bash
docker compose -f docker-compose.local.yml up -d
cp config.example.yaml config.yaml
$EDITOR config.yaml
env GOCACHE=/private/tmp/kbcore-gocache go run ./cmd/kbcore migrate-sql | \
  docker exec -i kbcore-postgres-local psql -U kbcore -d kbcore
```

Then run the local stack with LLM credentials configured in `config.yaml`:

```bash
env GOCACHE=/private/tmp/kbcore-gocache go run ./cmd/kbcore --config config.yaml serve --migrate-db
```

For managed remote indexing, configure the `graph` section in
`config.example.yaml`. Repository entries are the complete allowlist. Webhook
endpoints are `POST /webhooks/code/<registry-id>` and verify provider-specific
signatures/tokens before queueing only the configured branch. Clone credentials
are supplied to Git through an ephemeral HTTP header; they are not placed in
the remote URL, logs, snapshots, or API responses. Optional
`graph.semantic_enrichment` adds bounded LLM-inferred document relations after
the exact Go index completes; failures never replace or invalidate extracted
symbol/call facts.

## Service Mode

Start a local API server:

```bash
go run ./cmd/kbcore --config config.yaml serve
```

`serve` listens before the configured bootstrap corpus finishes. Follow progress
in the terminal or through `GET /workspace/status`; project APIs return HTTP 503
until Markdown validation and configured PostgreSQL/embedding sync are ready.
Each completed source is checkpointed in `.kbcore/source-manifest.json`, so a
restart skips unchanged sources and resumes at the first incomplete source.
Transient failures retry with exponential backoff configured by
`project.bootstrap.retry_initial_delay` and `retry_max_delay`. Use
`kbcore wait-ready` only when a script must wait for the complete project.

Add `--worker --scan-interval 30s` only when the service should continuously
scan `raw/sources/` and process queued files. Without `--worker`, writes happen
only through explicit API calls.

Common service calls:

```bash
curl -sS http://127.0.0.1:19829/health

curl -sS -X POST http://127.0.0.1:19829/sources/queue \
  -H 'Content-Type: application/json' \
  -d '{"source_path":"/path/to/new-file.md","title":"New File"}'

curl -sS -X POST http://127.0.0.1:19829/queue/run \
  -H 'Content-Type: application/json' \
  -d '{"agent":"llm","skip_unchanged":true}'

curl -sS -X POST http://127.0.0.1:19829/query \
  -H 'Content-Type: application/json' \
  -d '{"q":"女儿国 唐僧 八戒","agent":"llm","save_title":"女儿国 synthesis"}'

curl -sS 'http://127.0.0.1:19829/reviews?status=open'

curl -sS 'http://127.0.0.1:19829/projects/files?project=/tmp/demo-kb'

curl -sS -X POST http://127.0.0.1:19829/projects/sources/delete \
  -H 'Content-Type: application/json' \
  -d '{"project_path":"/tmp/demo-kb","source_path":"raw/sources/example.md","dry_run":true}'
```

The service is local-first. Set `server.api_token` to require
`Authorization: Bearer ...` for non-loopback requests; set
`server.api_require_token: true` to require the token for loopback requests too.

Agent clients can also use the stdio MCP server:

```bash
go run ./cmd/kbcore --config config.yaml mcp
```

## Web UI

The `web/` app is a Vite + React + TypeScript frontend using Ant Design. It
talks to `kbcore serve` through the Vite `/api` proxy during development.

```bash
go run ./cmd/kbcore --config config.yaml serve

cd web
npm install
npm run dev
```

Open the Vite URL; the UI reads the active project path, project id, and agent
from the backend and enters the configured project automatically.
The UI supports health checks, queueing sources, scanning `raw/sources/`, running
the ingest queue, querying with optional synthesis writeback, resolving review
items, linting, LLM wiki review, validate flow, and PostgreSQL sync.

Production build:

```bash
cd web
npm run build
```

`scripts/verify-xiyouji.sh` is the local deterministic validation path for the
split `tst/xiyouji-chapters` corpus. It creates a temporary project, runs mock
LLM Wiki ingest over all 100 chapter files, checks the expected `sources=100`,
`files=300`, and `reviews=100` shape, runs lint, then asks chapter-level
queries through internal deterministic query scaffolds and verifies the action
trace plus the first result. It also imports a tiny graphify snapshot and asks a
code relationship question that must use the `graph` tool. These scaffolds
exercise `list_pages`/`read`/`search`/`graph`/`final` tool-loop plumbing without
claiming real semantic LLM planning. The script checks navigation-first reads,
search fallback with auto-read evidence, and graphify evidence lookup.

To sync graphify/GitNexus-style graph facts into PostgreSQL, configure the
`database` section and run the commands with `--config config.yaml`; explicit
`--db-dsn` and `--project-id` remain available as one-off CLI overrides.

To run ingest/query with a real OpenAI-compatible or Anthropic Messages LLM
planner/synthesizer:

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
go run ./cmd/kbcore --config config.yaml validate-llmwiki --project /tmp/demo-kb --source ./tst/xiyouji-chapters/chapter-054.txt --agent llm
go run ./cmd/kbcore --config config.yaml query --project /tmp/demo-kb --q "女儿国 唐僧 八戒" --agent llm
go run ./cmd/kbcore --config config.yaml lint --project /tmp/demo-kb --agent llm
go run ./cmd/kbcore --config config.yaml review-wiki --project /tmp/demo-kb --agent llm
```

Set `llm.protocol: openai` for `POST /chat/completions`, or set
`llm.protocol: anthropic` for `POST /messages`. Anthropic mode sends
`x-api-key`, `Authorization: Bearer`, and `anthropic-version` (default
`2023-06-01`), which supports both Anthropic-native authentication and gateways
that retain Bearer authentication. `base_url` may be the provider root, a
`/v1` base, or the full operation URL; Anthropic root URLs are normalized to
`/v1/messages`. OpenAI-compatible root URLs retain the existing direct
`/chat/completions` behavior, while `/v1` bases use `/v1/chat/completions`.
The protocol setting is shared by ingest, overview synthesis, query
planning/tool use, and semantic wiki review.
Embeddings remain independently OpenAI-compatible through the `embedding`
section.

`llm.user_agent` is optional and defaults to `knowledge-core/0.1`. Gateways
that route or filter by client identity can override it; the configured value
is sent unchanged on both OpenAI-compatible and Anthropic requests.

`config.yaml` is the only runtime configuration source. Relative project and
bootstrap paths are resolved from the YAML file's directory. Explicit CLI flags
override YAML values; YAML values override program defaults. Legacy environment
variables and `.env.local`/`kbcore.env` files are intentionally ignored.

VS Code users can run the checked-in `Full Stack: LLM + Web` compound launch.
It starts `kbcore --config config.yaml serve` and the Vite frontend on
`127.0.0.1:5177`. While the backend performs the first LLM bootstrap, the web UI
shows per-source Analysis/Generation/persistence/sync progress and automatic
retry timing; it opens the configured project as soon as the API becomes ready.

Without valid YAML credentials, user-facing LLM commands fail fast with a clear
field-specific configuration error instead of silently falling back to offline scaffolds.

`scripts/verify-llmwiki-llm.sh` is the small real-LLM acceptance path. With a
valid YAML config, it creates three fixed source
files, runs `validate-llmwiki --agent llm`, checks that generated wiki pages
preserve source provenance and wikilinks, checks `overview.md` and
`reviews.md`, requires one `wiki/sources/` source-summary page per fixed source,
runs deterministic `lint` which must return `ok`, then runs
`review-wiki --agent llm` and `lint --agent llm`.
Because the fixture contains a deliberately stale compost-blend claim, both
LLM review commands must report at least one semantic issue type such as
`contradiction`, `stale-claim`, `source-gap`, or `review-needed`.
On success, it writes `VERIFY_REPORT.md` inside the temporary wiki project with
the fixed sources, generated pages, link count, and command outputs.
Pass the intended YAML path as the script's first argument; missing credentials
are a hard failure rather than an automatic mock fallback.
