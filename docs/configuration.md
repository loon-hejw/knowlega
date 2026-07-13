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
| `bootstrap.concurrency` | Configured bootstrap concurrency. Compilation remains bounded by write safety. |

### `llm`

Configures ingest, query planning/action use, overview synthesis, and semantic
review.

- `protocol`: `openai` for Chat Completions or `anthropic` for Messages.
- `base_url`: provider root, `/v1` base, or full operation URL.
- `api_key`, `model`: required for user-facing LLM commands.
- `user_agent`: optional gateway identity; defaults to
  `knowledge-core/0.1`.
- `anthropic_version`: defaults to `2023-06-01`.
- `timeout`, `retries`, `retry_base_delay`, `retry_max_delay`: request policy.
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

env GOCACHE=/private/tmp/kbcore-gocache \
  go run ./cmd/kbcore migrate-sql | \
  docker exec -i kbcore-postgres-local psql -U kbcore -d kbcore
```

Set `database.dsn` and `database.project_id` in `config.yaml`, then start with:

```bash
go run ./cmd/kbcore --config config.yaml serve --migrate-db
```

