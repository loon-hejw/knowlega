# Configuration

## Loading and Precedence

Knowledge Core reads runtime settings only from:

1. the path passed as global `--config PATH`; or
2. repository-root `config.yaml` by default.

Explicit CLI flags override YAML values, and YAML values override program
defaults. Relative `project.path`, bootstrap source, and related paths resolve
from the configuration file directory. Legacy environment variables and env
files are intentionally not runtime configuration sources.

Start from the checked-in template:

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
```

`config.yaml` is ignored by Git. Never commit real API keys, database DSNs,
webhook secrets, or provider tokens.

## Sections

### `project`

Defines the active workspace and optional service bootstrap.

| Field | Meaning |
| --- | --- |
| `name` | Project display/name seed. |
| `path` | Wiki project directory. |
| `bootstrap.source` | File or directory compiled while service starts. |
| `bootstrap.agent` | Bootstrap agent; real workflows use `llm`. |
| `bootstrap.reuse_existing` | Resume from existing Markdown and manifest state. |
| `bootstrap.retry_initial_delay` | First transient-failure retry delay. |
| `bootstrap.retry_max_delay` | Retry backoff ceiling. |
| `bootstrap.concurrency` | Maximum number of complete source-file tasks. A completed task is replaced immediately; this is not a stage batch size. |
| `bootstrap.max_task_attempts` | Per-source convergence guard; defaults to `4`. Exhaustion is a permanent bootstrap failure until the source or generation contract changes. |
| `bootstrap.max_conflict_attempts` | Separate optimistic-commit conflict allowance; defaults to `8` and does not consume ordinary failure attempts. |
| `bootstrap.max_impact_attempts` | Separate semantic impact re-integration allowance; defaults to `2`, so impact churn cannot consume the ordinary generation/conflict retry budget. |
| `bootstrap.max_files_per_task` | Maximum generated/updated wiki pages per source task; defaults to `12` to prevent page fragmentation. |
| `bootstrap.max_new_pages_per_source` | Lifetime maximum of new non-summary pages per source and generation contract; defaults to `3`. Existing canonical page updates do not count, and impact re-integration is update-only. |

`purpose.md` is part of the generation contract. The default is domain-neutral
and requires source-language output, evidence fidelity, reuse-first page
selection, and durable provenance. Empty files and the legacy initialization
placeholder are rejected before a real LLM request. Changing `purpose.md`,
`schema.md`, the pipeline version, or either page budget invalidates unchanged
manifest entries so the corpus is re-integrated under one consistent contract.
Source files larger than 64 MiB are rejected before in-memory extraction, and
multipart source uploads are limited to 128 MiB in total. Both HTTP and direct
service uploads enforce the aggregate limit while source archive bytes are
streamed to disk.

### `llm`

Configures ingest, query planning/action use, overview synthesis, and semantic
review.

- `protocol`: `openai` for Chat Completions or `anthropic` for Messages.
- `base_url`: provider root, `/v1` base, or full operation URL.
- `api_key`, `model`: required for user-facing LLM commands.
- `user_agent`: optional gateway identity; defaults to
  `knowledge-core/0.1`.
- `anthropic_version`: defaults to `2023-06-01`.
- `timeout`: per-request HTTP deadline.
- `concurrency`: provider request ceiling. `0` inherits `project.bootstrap.concurrency`; set it lower when the upstream model has less stable parallel capacity than the file-task scheduler.
- `operation_timeout`: total deadline for one logical LLM call including retries; when omitted, it inherits `timeout`.
- `retries`, `retry_base_delay`, `retry_max_delay`: retry policy within that total deadline.
- `max_input_chars`, `max_output_tokens`: prompt/response bounds.
- `disable_thinking`: asks compatible providers to suppress extended thinking.

Anthropic mode sends both Anthropic-native and Bearer-compatible headers so it
can work with native endpoints and compatible gateways. OpenAI-compatible `/v1`
bases resolve to `/v1/chat/completions`.

### `embedding`

Embeddings use an independently OpenAI-compatible endpoint. Set `model` to
enable embedding generation. Empty `base_url` or `api_key` values can inherit
the configured LLM endpoint/key where supported. `max_input_chars` truncates
oversized semantic page text before embedding.

Wiki embedding text contains title, type, aliases, sources, and body. A change
to any of these fields changes the source hash and refreshes the embedding on
the next relevant sync.

### `database`

- `dsn`: PostgreSQL connection string.
- `project_id`: stable PostgreSQL scope for the Markdown project.

Database use is optional. Configure PostgreSQL to enable durable operational
indexes, FTS, pgvector, graph facts, query logs, and review filtering. Markdown
remains authoritative.

### `server`

- `addr`: listen address, default `127.0.0.1:19829`.
- `agent`: default server agent.
- `worker`: opt-in ingest scan/queue worker.
- `scan_interval`: worker polling interval.
- `api_token`: Bearer token for protected API access.
- `api_require_token`: also require the token for loopback clients.

Non-loopback access requires a configured token. Keep the default loopback
binding unless the service is intentionally protected and exposed.

#### Knowledge Agent

Knowlega has no public Knowledge gRPC configuration. QM creates the internal
Agent at backend startup and derives each personal/project scope under
`knowledge.root_dir`. The Agent owns Markdown raw/wiki artifacts, the durable
ingest queue, page versions, lint/review, and derived PostgreSQL sync.

### `query`

Controls the single-agent deep-query loop. `max_steps` is the one hard limit
shared by model decisions, tool actions, and verification turns; it defaults to
256. The query has no total wall-clock deadline by default (`total_timeout:
0s`). A positive `total_timeout` remains available as an explicit operator
override. Per-request LLM operation timeouts and retries are configured under
`llm` and continue to apply.

`initial_action_budget`, `max_action_budget`, and `stagnation_rounds` are
deprecated compatibility keys and no longer divide or stop the action loop. If
`max_steps` is omitted, an explicitly configured legacy `max_action_budget` is
treated as `max_steps`. `verification_passes` still controls same-agent
coverage/adversarial stop reviews, and those turns count toward `max_steps`.

### `research`

Configures optional SearXNG-backed research jobs:

- `searxng_url`: SearXNG endpoint; leave empty to disable external search.
- `max_results`: bounded results per request.
- `timeout`: research request timeout.

### `graph`

Enables managed remote code indexing.

- `enabled`, `worker`: graph API/worker switches.
- `checkout_root`: managed repository checkout directory.
- `max_parallel_jobs`: graph-job concurrency limit.
- `semantic_enrichment`: optional bounded LLM-inferred relations after exact
  indexing.
- `registries`: explicit GitHub/GitLab/Gitea allowlists.

Each registry defines an ID, provider, base URL, credential sources, checkout
directory, and allowed repositories. Each repository has an ID, full name,
optional clone URL, branch, and disabled flag.

Registry credentials may be indirect environment references through
`api_token_env` and `webhook_secret_env`. These are narrowly scoped secret
lookups for remote Git integration, not general runtime configuration. Clone
credentials are passed through ephemeral Git HTTP headers and are not embedded
in remote URLs, logs, snapshots, or API responses.

## Local PostgreSQL

```bash
docker compose -f docker-compose.local.yml up -d

env GOCACHE=/private/tmp/knowlega-gocache \
  go run ./cmd/qm-backend --config qm-backend/configs/config.yaml
```

Set `database.url` and the QM/Knowledge LLM settings in
`qm-backend/configs/config.yaml`. `cmd/qm-backend` applies both QM migrations
and the isolated `knowledge_core` PostgreSQL schema at startup.
