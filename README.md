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
- `kbcore ingest`: copy an immutable source into `raw/sources/` and compile a
  source summary page into `wiki/sources/`. It also records the raw source and
  generated source page in `.kbcore/source-manifest.json`.
- `kbcore validate-llmwiki`: run the LLM Wiki ingest flow for one file or a
  directory of `.txt`/`.md` files, generating source/concept/entity/synthesis
  pages through strict `---FILE:`/`---REVIEW:` blocks. Pass `--agent llm` to use
  an OpenAI-compatible provider, or `--agent mock` for offline validation.
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
  an answer/citation bundle. The current default agent is an offline fallback;
  production should use the LLM action loop. Pass `--agent mock` for a
  deterministic offline tool-loop validation scaffold, or `--save-title` to
  write a useful LLM-backed answer back into `wiki/syntheses/`.
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
database configuration is available through `--db-dsn` or `KB_CORE_DB_DSN`, with
`--project-id` or `KB_CORE_PROJECT_ID` selecting the PG project scope. Query
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
Use `sync-wiki-pg --embed` with `KB_CORE_EMBEDDING_MODEL` plus
`KB_CORE_EMBEDDING_API_KEY` or `OPENAI_API_KEY` to populate page embeddings.
When `ingest`, `validate-llmwiki`, `query --save-title`, or
`code-import-graphify` run with `--db-dsn`/`--project-id`, the pages they write
are incrementally synced into PG; if embedding env vars are configured, those
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
    code-graphs/
  wiki/
    index.md
    log.md
    overview.md
    reviews.md
    sources/
    code/
    syntheses/
```

## Build And Test

```bash
go test ./...
bash scripts/verify-xiyouji.sh
go run ./cmd/kbcore init --path /tmp/demo-kb --name demo
go run ./cmd/kbcore ingest --project /tmp/demo-kb --source ./README.md
go run ./cmd/kbcore validate-llmwiki --project /tmp/demo-kb --source ./tst/xiyouji-chapters --agent mock --skip-unchanged
go run ./cmd/kbcore queue-ingest --project /tmp/demo-kb --source ./tst/xiyouji-chapters/chapter-054.txt
go run ./cmd/kbcore run-queue --project /tmp/demo-kb --agent mock
go run ./cmd/kbcore query --project /tmp/demo-kb --q graph
go run ./cmd/kbcore lint --project /tmp/demo-kb
bash scripts/verify-llmwiki-llm.sh
```

`scripts/verify-xiyouji.sh` is the local deterministic validation path for the
split `tst/xiyouji-chapters` corpus. It creates a temporary project, runs mock
LLM Wiki ingest over all 100 chapter files, checks the expected `sources=100`,
`files=300`, and `reviews=100` shape, runs lint, then asks chapter-level
queries through `query --agent mock` and verifies the action trace plus the
first result. It also imports a tiny graphify snapshot and asks a code
relationship question that must use the `graph` tool. The mock query agent
exercises `list_pages`/`read`/`search`/`graph`/`final` tool-loop plumbing without
claiming real semantic LLM planning. The script checks navigation-first reads,
search fallback with auto-read evidence, and graphify evidence lookup.

To sync graphify/GitNexus-style graph facts into PostgreSQL and let query use
PG graph evidence:

```bash
export KB_CORE_DB_DSN='postgres://user:pass@localhost:5432/kb?sslmode=disable'
export KB_CORE_PROJECT_ID=demo
go run ./cmd/kbcore code-import-graphify --project /tmp/demo-kb --repo-id repo --repo-path /path/to/repo --graph graph.json --db-dsn "$KB_CORE_DB_DSN" --project-id "$KB_CORE_PROJECT_ID"
go run ./cmd/kbcore sync-wiki-pg --project /tmp/demo-kb --db-dsn "$KB_CORE_DB_DSN" --project-id "$KB_CORE_PROJECT_ID" --migrate-db --embed
go run ./cmd/kbcore query --project /tmp/demo-kb --project-id "$KB_CORE_PROJECT_ID" --db-dsn "$KB_CORE_DB_DSN" --q "ValidateToken AuthService" --agent llm
```

To run ingest/query with a real OpenAI-compatible LLM planner/synthesizer:

```bash
export KB_CORE_LLM_BASE_URL=https://api.openai.com/v1
export KB_CORE_LLM_API_KEY=...
export KB_CORE_LLM_MODEL=...
export KB_CORE_EMBEDDING_MODEL=text-embedding-3-small
go run ./cmd/kbcore validate-llmwiki --project /tmp/demo-kb --source ./tst/xiyouji-chapters/chapter-054.txt --agent llm
go run ./cmd/kbcore query --project /tmp/demo-kb --q "女儿国 唐僧 八戒" --agent llm
go run ./cmd/kbcore query --project /tmp/demo-kb --q "女儿国 唐僧 八戒" --agent llm --save-title "女儿国 synthesis"
go run ./cmd/kbcore lint --project /tmp/demo-kb --agent llm
go run ./cmd/kbcore review-wiki --project /tmp/demo-kb --agent llm
```

Without those variables, `--agent auto` uses the offline mock/fallback agents.
Those are useful for tests, but they are not the final LLM Wiki behavior and
their query answers are not eligible for `--save-title` writeback.

`scripts/verify-llmwiki-llm.sh` is the small real-LLM acceptance path. It skips
when LLM credentials are absent. When configured, it creates three fixed source
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
Set `KB_CORE_REQUIRE_LLM=1` when this should be a hard gate instead of a skip on
missing LLM credentials.
