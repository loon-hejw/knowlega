# Architecture

## Platform Direction

The long-term integration direction is to use QM as the collaboration and
access-control substrate, with Knowledge Core as a project and personal
knowledge plugin. QM owns users, projects, memberships, permissions, channels,
tasks, schedules, approvals, and agent runtime. Knowledge Core owns immutable
source ingestion, durable Markdown Wiki pages, provenance, deterministic
knowledge facts, lint, review, and knowledge maintenance. QM owns the
user-facing reasoning loop.

This is a boundary decision, not a second persistence model: QM scope identifiers
must resolve to a controlled Knowledge Core project/scope, while `raw/`, `wiki/`,
`.kbcore/`, and optional PostgreSQL state remain owned by Knowledge Core. Personal
knowledge is private by default; promotion into a project or organization scope
is explicit and provenance-preserving.

See [ADR-0001](adr/0001-qm-platform-knowledge-core-plugin.md) for the scope
model, plugin contract, security boundary, deployment shape, and phased roadmap.

## System Model

Knowledge Core maintains a persistent wiki rather than generating disposable
answers from retrieved chunks. Each layer has a distinct responsibility:

| Layer | Responsibility | Source of truth |
| --- | --- | --- |
| `raw/` | Immutable imported evidence | Yes |
| `wiki/` | Evolving Markdown knowledge and navigation | Yes |
| `.kbcore/` | Manifests, queues, archived page versions, graph snapshots | Durable operational state |
| PostgreSQL | Search, graph, cache, jobs, and review indexes | No; rebuildable |
| QM outer model | User-facing planning and synthesis through tools | No; may request explicit validated writeback |
| Maintenance LLM | Ingest, research, semantic review, graph enrichment | No; writes validated artifacts |

`purpose.md` and `schema.md` guide ingest, knowledge use, and semantic review. Generated
wiki pages use YAML frontmatter, source provenance, and body `[[wikilink]]`
references. A generated page overwrite archives the previous version under
`.kbcore/page-versions/`.

## Ingest and Compilation

Ingest follows an analysis-then-generation pipeline:

1. Archive the source under `raw/sources/<collection>/<slug>-<hash12>/`.
2. Read the source with project purpose, schema, index, and overview.
3. Ask the LLM to analyze the source before choosing page updates.
4. Generate strict `---FILE:` and `---REVIEW:` blocks.
5. Validate paths, frontmatter, provenance, and wikilinks.
6. Merge versioned Markdown writes and update navigation artifacts.
7. Record source hashes and generated pages in
   `.kbcore/source-manifest.json`.
8. Incrementally sync written artifacts to PostgreSQL when configured.

Every source commit is a recoverable multi-file transaction. Before changing
generated pages, index/log/reviews, or the manifest, the compiler writes a
durable rollback journal under `.kbcore/transactions/`. An in-process error
rolls every active artifact back; restart recovery rolls back any prepared
transaction left by a process interruption. Rollback also removes page-version
archives created by the failed attempt, so aborted writes cannot appear later
as valid historical evidence. A project-scoped filesystem lock
prevents a second process from writing the same knowledge base concurrently.

One source can update multiple source, concept, entity, synthesis, index, and
overview pages. Review blocks are persisted to `wiki/reviews.md`; PostgreSQL
review rows are derived from that file.

Analysis, generation, validation repair, and persistence share one explicit
generation contract. The manifest records its hash together with the source's
new-page budget and creation ledger. A source may create at most three durable
non-summary pages per contract; impact-driven re-integration updates existing
pages only. A purpose/schema or policy change invalidates the old contract and
re-integrates every source. A source that still fails after its configured task
attempt limit stops bootstrap as a non-retryable failure instead of entering an
unbounded service retry loop.

The manifest is decoded and written through one shared schema. Re-importing a
logical source path with different bytes creates a new immutable raw archive;
the current entry points at the new version while `versions[]` preserves prior
hashes and raw paths. Shared pages retain version provenance, while the live
source-summary represents only the current version. `page_owners` distinguishes
source-managed pages from manual, review, code, and synthesis pages, so convergence
never treats an unknown human page as disposable. Pages attributed only to a
retained historical source version remain source-owned; historical summaries
are still superseded by the single current source-summary.

LLM concurrency is a separately configurable process-wide request ceiling
shared by compile, impact, research, review, and graph clients. Complete source tasks generate from
consistent snapshots without holding page locks during LLM work. A stale commit
requeues only that source; conflict retries with overlapping paths are isolated,
while disjoint retries continue in parallel. Semantic impact retries use a
separate bounded allowance and require exact source and post-change evidence.

The durable queue is `.kbcore/ingest-queue.json`. QM project-file uploads create
an immutable raw mirror and enqueue work without blocking the upload response.
The project-file worker runs by default, uses a per-scope maintenance lock plus
the filesystem project lock, requeues interrupted `processing` tasks on
restart, and retries failed queue tasks idempotently. Generic non-project
raw-directory scanning remains separately opt-in. The source manifest enables
restart and unchanged-source skipping.

Workspace state is derived from Markdown, raw sources, the source manifest, and
the queue rather than a second readiness flag. It is one of `empty`, `queued`,
`processing`, `ready`, or `failed`, with queue counts, the latest error, and the
last successful compile time. PostgreSQL scope rows mirror this state for
operations, but are not authoritative. A queued/processing search with no hits
returns `pending`; it cannot be used as evidence that the corpus lacks a fact.

## Knowledge Tool and Writeback

QM's outer Pi loop is the only user-facing reasoner. Knowledge Core exposes one
deterministic `knowledge` tool with these actions:

- `status`, `search`, `discover`, and `list` for status/navigation;
- `read`, `follow_links`, and `graph` for evidence;
- `submit` to validate the current turn's candidate, requirements, checks, and
  evidence ledger; and
- `writeback` to save an already validated submission when explicitly requested.

There is no nested Knowledge query model, hidden classifier,
candidate-hypothesis/audit chain, verifier, or fixed pass count. Search snippets,
candidate lists, and aggregate navigation pages are not evidence. Citations are
reconstructed only from read/follow/graph results produced after the current
user entry.

Recall order is:

1. pgvector when embeddings and a vector store are configured;
2. PostgreSQL full-text search;
3. Markdown and immutable raw-source file scanning;
4. lower-priority wiki/code graph expansion.

Raw sources and generated pages participate in one result set. Chinese
punctuation and whitespace are normalized, the complete phrase is retained,
and continuous CJK text receives general 2-gram/3-gram terms. Title, aliases,
sources, and body use aligned scoring; aliases and titles are boosted while
aggregate navigation pages are downranked. There are no corpus-specific search
aliases or hard-coded semantic expansions.

`WriteKnowledgeSubmission` permits a page under `wiki/syntheses/` only for a
complete validated submission with at least one non-navigation citation. The
user must explicitly request writeback. Saved pages include the original
question and citation sources, and repeated identical submissions are
idempotent.

## Wiki Maintenance

Structural and semantic checks remain separate:

- `lint` deterministically checks broken links, orphan pages, missing outlinks,
  and known title/alias mentions that should be wikilinks.
- `lint --agent llm` runs structural lint followed by semantic review.
- `review-wiki --agent llm` finds contradictions, duplicates, missing pages,
  stale claims, and source gaps.

Semantic review receives project guidance, navigation pages, wiki excerpts,
the source manifest, and raw excerpts. Review status changes are written back
to `wiki/reviews.md` before PostgreSQL is updated.

## Code and Wiki Graphs

GitNexus-style exact facts are the primary code evidence. Nodes can represent
repositories, folders, files, packages, types, interfaces, functions, methods,
routes, tools, communities, and processes. Edges capture containment,
definitions, calls, imports, implementations, routing, tool handling, and
process flow.

`code-index-go` builds a native Go graph and records commit, dirty-worktree,
and indexed-source hash metadata. `code-import-graphify` imports portable graph
snapshots and optional reports. Repository-scoped stable IDs prevent collisions
between multiple repositories.

Graph sync is repo-scoped replacement inside a transaction when the store
supports it. Graphify-style inferred or ambiguous evidence supplements exact
symbol/call facts; it does not override them. Query graph evidence can combine
code graph facts with wiki links, shared sources, common neighbors, and type
affinity.

## PostgreSQL Boundary

PostgreSQL stores projects, sources, source manifests, wiki pages and versions,
review items, historical query indexes, jobs, code repositories, graph nodes/edges, FTS, and
pgvector embeddings.

Full wiki sync treats PostgreSQL as replaceable derived state:

- removed Markdown page rows are pruned;
- page-version rows mirror `.kbcore/page-versions/`;
- review rows mirror `wiki/reviews.md`;
- source/manifest rows mirror `.kbcore/source-manifest.json`, including one
  immutable `sources` row for every retained source version.

Incremental sync only upserts artifacts written by the current operation and
does not prune unrelated rows. Wiki page and embedding updates use a store
transaction when supported. Embeddings refresh only when the model or semantic
embedding-source hash changes.
