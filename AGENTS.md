# AGENTS.md

This project is a Go/PostgreSQL/Markdown implementation of an LLM-maintained
knowledge base. Keep development aligned with these reference systems:

- Karpathy LLM Wiki:
  https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f#llm-wiki
- Local `llm_wiki/`
- Local `GitNexus/`
- Local `graphify/`

## Core Direction

Do not build this as ordinary RAG. The core idea is a persistent, compounding
wiki maintained by an LLM:

- `raw/` contains immutable source material.
- `wiki/` contains generated Markdown pages that evolve over time.
- `schema.md` and `purpose.md` guide every ingest, query, and lint operation.
- PostgreSQL is an index/state/graph/cache layer, not the only knowledge source.
- Markdown artifacts must remain readable by humans, agents, Git, and Obsidian.
- Generated page overwrites must preserve the previous page under
  `.kbcore/page-versions/`; do not silently destroy earlier wiki evidence.

## LLM Wiki Rules

Follow the Karpathy LLM Wiki pattern:

- Ingest should compile sources into durable wiki pages, not only index chunks.
- One source may update many pages: source summary, entity pages, concept pages,
  synthesis pages, index, overview, and log.
- Query results that create useful synthesis should be saveable back into wiki.
- Saved query answers belong under `wiki/syntheses/` with `type: synthesis`,
  citation sources, the original question, and the query plan used to produce
  the answer.
- Lint should check wiki health: contradictions, stale claims, orphan pages,
  missing cross-links, missing concept/entity pages, and source gaps.
- Keep structural lint and semantic review separate. `kbcore lint` is the
  deterministic guardrail for broken links, orphan pages, missing outlinks, and
  known title/alias mentions. `kbcore lint --agent llm` runs those structural
  checks plus the LLM semantic review. `kbcore review-wiki --agent llm` is the
  LLM Wiki maintenance pass for contradictions, duplicates, missing pages, stale
  claims, and source gaps. The LLM review context must include project guidance,
  navigation pages, page excerpts, the source manifest, and raw source excerpts
  so stale claims and source gaps can be checked against source evidence.
- Lint should use known page titles and `aliases` frontmatter to find mentions
  that should become `[[wikilink]]` cross-references.
- `---REVIEW:` blocks from ingest must be persisted to `wiki/reviews.md`; do
  not discard them after counting.
- Review task status changes must also be persisted to `wiki/reviews.md`;
  PostgreSQL review state remains derived from the Markdown artifact.
- `wiki/index.md` is the content catalog and LLM navigation entrypoint.
  Maintain it by sections such as Sources, Concepts, Entities, Syntheses, and
  Code; do not append every generated page into an undifferentiated tail.
- `wiki/log.md` is append-only operational history.
- `wiki/overview.md` is the current high-level synthesis of the whole wiki.
  Ingest and saved query workflows must update it so the LLM has a compact
  navigation/synthesis entrypoint before drilling into individual pages.
- Generated pages must use YAML frontmatter and body `[[wikilink]]` references.

The validation command, `kbcore validate-llmwiki`, supports both an offline mock
and an OpenAI-compatible LLM provider. Treat the mock as a scaffold only; real
ingest should use an LLM provider whenever credentials are available:

1. Analysis: source + purpose + schema + index + overview.
2. Generation: strict `---FILE:` and `---REVIEW:` blocks.
3. Validation: frontmatter, paths, source provenance, wikilinks.
4. Merge: update existing pages without destroying prior evidence.

Local runtime settings are read only from the repository-root `config.yaml`, or
from the path passed with global `--config`. CLI flags override YAML values,
which override program defaults. Do not read runtime settings from environment
variables or legacy env files, and do not commit `config.yaml` or real API keys.

For small end-to-end LLM acceptance, use `scripts/verify-llmwiki-llm.sh`. It
creates three fixed source files, runs `validate-llmwiki --agent llm`, checks
generated pages, links, `wiki/overview.md`, `wiki/reviews.md`, deterministic
`lint` returning `ok`, `review-wiki --agent llm`, and `lint --agent llm`. It
must also require one `wiki/sources/` source-summary page per fixed source. It
must receive a YAML config with real LLM credentials and fail clearly when that
configuration is missing.
Because the fixture contains a deliberately stale compost-blend claim, both LLM
review commands must report at least one semantic issue type. Successful runs
write `VERIFY_REPORT.md` inside the temporary wiki project with source,
generated-page, link, lint, and review evidence.

## Lessons From Local `llm_wiki`

Use local `llm_wiki` as the product/workflow reference, not as a technology
stack to copy. This project intentionally avoids Node for the core.

Borrow these behaviors:

- `purpose.md` is first-class project intent.
- Two-stage ingest: analysis first, page generation second.
- `sources[]` frontmatter links every generated page back to raw material.
- Review items capture contradictions, duplicates, missing pages, and human
  judgment calls.
- Source watch and content hash enable incremental ingest.
- `queue-ingest`, `scan-sources`, and `run-queue` are the Go core's durable
  queue/watch substitute. Keep `.kbcore/ingest-queue.json` recoverable and
  aligned with `.kbcore/source-manifest.json`; do not make queue state a second
  knowledge source of truth.
- `.kbcore/source-manifest.json` records source hashes and generated pages.
  Both deterministic `ingest` and LLM `validate-llmwiki` must update it. Batch
  ingest should use `--skip-unchanged` when rerunning large corpora so LLM
  work is only spent on changed sources.
- Query should combine lexical search, source search, graph expansion, context
  budgeting, and citations.
- Query must be planner-driven. Lexical/FTS/vector/graph search is only a
  candidate recall tool; the user-facing query workflow should have an LLM
  planning step, read `wiki/index.md` first, inspect relevant wiki pages/raw
  sources, synthesize with citations, and optionally write valuable answers
  back into the wiki.
- The LLM query planner may choose direct `read_first` pages without running
  search. Search should never be the controlling workflow; it is one tool the
  planner can call when navigation pages and graph context are insufficient.
- LLM-backed query agents should use the bounded action loop: `read` wiki/raw
  pages, `list_pages` for wiki navigation, `follow_links` to traverse linked
  pages, `search` for candidate recall, `graph` for imported graphify/GitNexus
  evidence, then `final` or `writeback` only after enough evidence has been
  read. `writeback` records a suggested synthesis title; it does not write files
  unless the caller explicitly requests save-title/auto save-title. `search`
  should prefer pgvector when an embedding provider and vector store are
  configured, then PG `wiki_pages.search_vector`, and finally file scanning only
  when PG has no evidence. `graph` should prefer a PG `GraphEvidenceStore` when
  configured and fall back to `raw/code-graphs/` snapshots only when PG has no
  evidence. `list_pages` is navigation only; it is not final-answer evidence.
  Navigation observations must be passed to the next LLM action separately from
  evidence documents. The runtime auto-reads top wiki/raw candidates after
  `search` and rejects `final` when only aggregate navigation, page lists, or
  search snippets are available. Do not let search snippets become the answer
  without a read or graph evidence step. Query citations must be built from read
  evidence documents only, never directly from raw search results.
- `read` actions may use project-relative wiki/raw paths, relative wiki links,
  exact wiki titles, `[[wikilink]]` names, or aliases. The runtime must resolve
  those names back to safe Markdown paths before reading.
- Query writeback must be enforced in `WriteQueryAnswer`, not only in CLI/API
  callers. Only non-offline answers with `can_write_back=true` and at least one
  non-navigation citation may be saved under `wiki/syntheses/`.
- Query workflows configured with PostgreSQL should append to `query_logs`.
  Treat questions as useful maintenance signals for missing pages, recurring
  synthesis needs, and review follow-up.
- CLI/API database configuration is available through `database.dsn` and
  `database.project_id`, with explicit `--db-dsn`/`--project-id` CLI overrides.
- Embedding generation must be injected through a real provider. Tests may use
  fake vectors to verify plumbing, but do not hard-code deterministic fake
  embeddings as production semantics.
- Wiki page embedding text must include semantic frontmatter that affects
  recall: title, type, aliases, sources, and body. Alias/source changes should
  change `embedding_source_sha256` and refresh the embedding.
- OpenAI-compatible embedding configuration uses the `embedding` YAML section;
  an enabled embedding model may inherit the configured LLM base URL/API key.
- Use `sync-wiki-pg --embed` to populate `wiki_pages.embedding` from current
  Markdown pages. Query can then use pgvector before PG FTS and file fallback.
- Full `sync-wiki-pg` treats PostgreSQL wiki rows as a rebuildable index:
  delete `wiki_pages` rows for paths that no longer exist in Markdown. Incremental
  sync after `ingest`, `validate-llmwiki`, or query writeback must only update
  the pages it wrote and must not prune unrelated rows.
- Full `sync-wiki-pg` should import `.kbcore/page-versions/` archives into
  `wiki_page_versions`, preserving previous page content, sources, reason, and
  archive timestamp in PostgreSQL. Full sync should prune stale PG version rows
  no longer present under `.kbcore/page-versions/`; incremental page sync should
  leave archived versions alone.
- `wiki/reviews.md` is the durable review artifact. Sync review entries into
  PG `review_items` for filtering and operational follow-up; full sync should
  prune stale PG review rows that are no longer present in Markdown, while
  incremental sync should only upsert current reviews.
- `.kbcore/source-manifest.json` is the durable source processing manifest.
  Sync it into PG `source_manifest`; full sync should prune stale PG manifest
  rows, while incremental sync after writes should only upsert current manifest
  entries. Use the same manifest to sync PG `sources` rows for immutable raw
  source provenance, including raw path, original path, title, SHA256, and
  import time.
- When `ingest`, `validate-llmwiki`, `query --save-title`, or
  `code-import-graphify` run with PG configuration, they should incrementally
  sync the pages they write into PG. If embedding env vars are configured, those
  page embeddings should refresh in the same pass only when the embedding text
  hash or embedding model changed. Track freshness with `embedding_model`,
  `embedding_source_sha256`, and `embedding_updated_at`.
- API write paths should keep parity with CLI write paths: when `serve` is
  configured with a PG-backed wiki store and project id, ingest and query
  writeback should sync written wiki pages, source manifest entries, and raw
  source provenance into PG before returning success. Code graph imports should
  also sync graph facts and the generated code overview page before returning
  success.
- Service mode is the primary integration surface for applications. Keep HTTP
  handlers thin and reuse the same compiler/service/wiki functions as CLI
  commands. Do not create separate service-only persistence formats.
- `serve --worker` may scan `raw/sources/` and consume the ingest queue, but it
  must stay opt-in so starting the service does not unexpectedly spend LLM
  tokens.
- Configured service bootstrap should listen before a large corpus finishes,
  expose progress through workspace status, and resume from the source manifest
  after restart. Keep bootstrap compilation serial unless aggregate Markdown
  and manifest writes are made concurrency-safe; PostgreSQL and embedding sync
  must be idempotently completed before the project is marked ready.
- The `web/` frontend is a Vite + React + TypeScript + Ant Design application.
  Keep it as an API client over `kbcore serve`; do not duplicate LLM Wiki
  compile, query, review, or persistence logic in the browser.
- Wiki page sync should run inside `WithWikiPageStoreTx` when the backing store
  supports it, so page rows and embedding metadata do not partially commit on
  sync errors. The remaining production extension is making these PG sync
  transactions share boundaries with the corresponding Markdown writes where
  practical.
- Code graph sync should run inside `WithCodeGraphStoreTx` when the backing
  store supports it, so imported graphify/GitNexus repo, node, and edge facts do
  not partially commit on sync errors. Sync is repo-scoped replacement: delete
  old graph facts for the repo before inserting the current snapshot.
- Do not hard-code corpus-specific aliases in the search scorer. Aliases and
  semantic expansions belong in page frontmatter, graph facts, the index, or the
  LLM query plan.
- Candidate recall must read `aliases` frontmatter as structured metadata and
  boost it over repeated body-term frequency. This preserves the LLM Wiki model:
  the wiki stores semantic naming decisions; search only uses them. Keep file
  search, PG `wiki_pages.search_vector`, and PG fallback scoring aligned on
  alias handling.
- `FallbackQueryAgent` is only for offline tests. When API credentials are
  available, query should use the OpenAI-compatible `QueryAgent` so planning and
  synthesis are actually LLM-driven. Fallback query answers must keep
  `can_write_back=false`; only LLM-backed synthesis should be saved to
  `wiki/syntheses/`.
- `MockQueryAgent` is a deterministic offline tool-loop scaffold for local
  validation only. It may exercise `list_pages`, `read`, `search`, and `final`,
  but it must keep `can_write_back=false` and must not be treated as semantic
  LLM reasoning.
- Graph relevance should consider direct links, source overlap, common
  neighbors, and type affinity.
- Ordinary wiki graph relevance is part of query evidence expansion. The
  `graph` action should combine code graph evidence with wiki page graph
  evidence when both are available.

Avoid reducing the system to deterministic summaries. Deterministic behavior is
acceptable for tests and scaffolding, but the design target is an LLM wiki
maintainer.

## GitNexus Role

GitNexus is the reference for code knowledge graph depth.

For code repositories, prefer semantic graph facts over plain text search:

- Nodes should model files, folders, functions, classes, methods, routes, tools,
  processes, and communities.
- Edges should model `CONTAINS`, `DEFINES`, `CALLS`, `IMPORTS`, `EXTENDS`,
  `IMPLEMENTS`, `HANDLES_ROUTE`, `HANDLES_TOOL`, `MEMBER_OF`, and process flow.
- Code questions should support query, context, impact, trace, and stale-index
  checks.
- Store commit/index metadata so stale code wiki pages can be detected.
- Code wiki pages must cite repo, commit, symbol ids, and graph source.

GitNexus-style code graph facts are the primary code evidence layer. Do not let
freeform LLM summaries override exact symbol/call/impact facts.

## Graphify Role

Graphify is the reference for lightweight graph snapshots and readable graph
reports.

Use graphify-style outputs as supplemental evidence:

- `graph.json` is a portable graph exchange format.
- `GRAPH_REPORT.md` is a human/agent-readable audit report.
- Confidence labels matter: `EXTRACTED`, `INFERRED`, `AMBIGUOUS`.
- Community detection, god nodes, surprising connections, and suggested
  questions are useful wiki seeds.
- Graphify is especially useful for cross-document and mixed corpus context.

Graphify supplements GitNexus for code. It should not replace exact GitNexus
semantic facts when both are available.

## Implementation Rules

- Primary language: Go.
- Avoid Node in the core implementation.
- Keep dependencies conservative; prefer standard library where practical.
- Keep Markdown files as durable artifacts and PostgreSQL as derived state.
- Rebuild PostgreSQL wiki page state from Markdown with `wiki.ScanWikiPages`
  and batch writes through `postgres.Store.UpsertWikiPages`; do not create a
  second source of truth in PG.
- Sync code graph snapshots into PG with `service.SyncCodeGraphSnapshot`. Graph
  fact IDs must be project/repo-scoped stable IDs, not raw graphify/GitNexus ids,
  so multiple repos cannot collide in `graph_nodes` or `graph_edges`.
  `SyncCodeGraphSnapshot` should use `WithCodeGraphStoreTx` when available and
  replace old facts for that repo before inserting the current snapshot.
- PG schema should support projects, sources, wiki pages, page versions, graph
  nodes/edges, code repos, jobs, source manifests, review items, query logs,
  FTS, and pgvector.
- Any generated file path must stay inside the project root.
- Any generated wiki file must be Markdown under `wiki/`.
- Raw source files must not be mutated after import.
- Wiki page writes should use versioned writes so previous content remains
  available for review and PG page-version sync.
- Prefer tests that prove behavior through real temporary project directories.
- Use `/private/tmp/...` for Go build caches in restricted environments:
  `env GOCACHE=/private/tmp/kbcore-gocache go test ./...`

## Validation Corpus

`tst/xiyouji.txt` is split into:

```text
tst/xiyouji-chapters/chapter-001.txt
...
tst/xiyouji-chapters/chapter-100.txt
```

Use this corpus to test multi-source wiki accumulation:

```bash
env GOCACHE=/private/tmp/kbcore-gocache go run ./cmd/kbcore --config config.yaml init \
  --path /private/tmp/kbcore-xiyouji-wiki --name xiyouji

env GOCACHE=/private/tmp/kbcore-gocache go run ./cmd/kbcore --config config.yaml validate-llmwiki \
  --project /private/tmp/kbcore-xiyouji-wiki \
  --source tst/xiyouji-chapters \
  --agent mock
```

Expected validation shape:

- 100 sources.
- 300 generated wiki pages.
- No broken `[[wikilink]]` lint issues.
- Query should search wiki pages and `raw/sources/`, while downranking
  aggregate pages such as `wiki/index.md`, `wiki/log.md`, and `wiki/overview.md`.
