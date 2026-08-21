# Configuration

## Loading and precedence

`cmd/qm-backend` reads the QM YAML passed with `--config PATH`; its default is
the ignored local path `configs/qm-config.yaml`. New deployments should keep
database, model, worker, file-store, authentication, and Knowledge Agent
settings in that one file.

Start from the checked-in template:

```bash
cp configs/qm-config.example.yaml configs/qm-config.yaml
$EDITOR configs/qm-config.yaml
```

Never commit the local file, real API keys, database DSNs, signing secrets, or
provider tokens. A limited set of legacy QM environment variables is still
translated for migration compatibility, but it is not the configuration model
to extend.

## `server`

- `http_addr`: QM HTTP listener; defaults to `:18083`.
- `grpc_addr`: internal QM control/runner gRPC listener.

Health and readiness are available at `/healthz` and `/readyz`. Readiness means
the process and database are serving; it does not wait for every project wiki
to finish compiling.

## `database`

`database.url` is required. `qm-backend` applies the public QM migrations and
the isolated `knowledge_core` schema at startup. Markdown remains authoritative;
the Knowledge schema is a rebuildable search, graph, review, source-manifest,
and page-version index.

## `qm`

Core fields:

- `org_id`: organization identifier used for QM and Knowledge scope bindings.
- `node_core_url`: compatibility upstream while routes are still proxied.
- `route_mode`: `proxy`, `shadow_read`, or `go`.
- `agent_workspace_root`: parent directory for per-scope tool workspaces.
- `sandbox_default_backend` and `sandbox_backends`: declared sandbox choices.
  Local sandbox execution requires Docker and never falls back to the host.

### `qm.file_store`

The current Go-owned artifact store uses:

- `mode: local`;
- `local_dir` for durable content-addressed QM file blobs; and
- `transfer_local_dir` for short-lived staged uploads.

QM owns file artifacts, project memberships, authorization, and deletion.
Knowledge receives an immutable read-only mirror only after those writes
succeed.

### `qm.slack` and `qm.oauth`

Slack bot/app tokens and OAuth client declarations live in the shared QM YAML.
OAuth clients declare provider, client credentials, scopes, redirect allowlist,
consent mode, and optional hosted domain. Deprecated flattened OAuth/Slack
catalog fields are rejected.

## `qm.models`

This is the only model-selection source for QM turns and Knowledge maintenance.
The outer QM Harness is the only query reasoner; Knowlega reuses the selected
provider for ingest, overview synthesis, and semantic review.

- `default_harness`: deployment-wide default Harness.
- `request`: shared per-attempt timeout, total operation timeout, retries,
  input/output bounds, and optional thinking suppression.
- `providers[]`: deployment-specific OpenAI, Anthropic, or local `mock` wire
  providers with models and credentials.
- `harnesses[]`: binds `pi`, `opencode`, `codex`, `claude`, or `mock` to one
  provider and an allowed model set.

Each provider model may declare `context_window`, `max_tokens`, `fast_mode`,
and `adaptive_thinking`. Provider IDs are deployment names, not subscription
choices. The UI exposes only the Harness/model combinations declared here.

The production Harnesses share the Go agent engine while retaining distinct
transports:

| Harness | Provider wire | Runtime dependency |
| --- | --- | --- |
| `pi` | OpenAI or Anthropic | direct Go HTTP transport |
| `opencode` | OpenAI or Anthropic | OpenCode runtime |
| `codex` | OpenAI | Codex CLI/app-server |
| `claude` | Anthropic | Claude CLI stream JSON |

Harness `runtime` settings include executable paths where applicable, startup
and wall-clock limits, optional detection/title/judge models, request capture,
cache splitting, and controlled execution capabilities. Configuration
validation rejects incompatible Harness/provider pairs.

The legacy `knowledge.llm` block is accepted only as a one-time in-memory
migration when `qm.models` is absent. New deployments must use `qm.models`.

For an OpenAI-compatible model gateway, declare every selectable model in the
same provider and allow it on the `pi` Harness. For example:

```yaml
qm:
  models:
    default_harness: pi
    providers:
      - id: company-modelgate
        protocol: openai
        base_url: https://model-gateway.example/v1
        api_key: replace-with-your-key
        models:
          - id: k2.6
            name: K2.6
          - id: qwen3.8-27b-fp8
            name: Qwen3.8 27B FP8
    harnesses:
      - id: pi
        provider: company-modelgate
        model_ids: [k2.6, qwen3.8-27b-fp8]
        default_model: qwen3.8-27b-fp8
```

The model ID is sent to the gateway exactly as configured. Keep vendor API
keys only in the ignored `configs/qm-config.yaml` file.

## `knowledge`

Knowlega has no standalone listener or standalone project configuration. QM
creates the internal Agent and derives every managed project scope below
`knowledge.root_dir`.

- `root_dir`: parent directory for managed Knowledge scopes.
- `auto_process_project_files`: defaults to `true`; asynchronously compiles QM
  uploads and attachments without blocking upload responses.
- `worker`: opt-in generic scan for non-project `raw/sources/` workspaces.
- `scan_interval_seconds`: polling interval for both maintenance paths.

Project files progress through `queued`, `processing`, `ready`, or `failed`;
a project with no sources is `empty`. Raw evidence remains searchable while
derived pages are pending. Disabling the generic worker does not stop normal QM
project files from becoming wiki knowledge.

There is no `knowledge.query` configuration. Such a block is rejected because
Pi controls navigation and evidence gathering through the deterministic
`knowledge` tool.

## `workers`

`qm-backend` runs categorized durable workers inside the same process.
`workers.reap_interval_seconds` controls stale-lease recovery and
`workers.pools.<name>` may set:

- `concurrency`;
- `lease_ttl_seconds`;
- `heartbeat_interval_seconds`;
- `poll_interval_millis`; and
- `max_claim_backoff_millis`.

The `turn` pool serializes work per session while allowing unrelated sessions
to run concurrently. The `sandbox` pool handles container-backed execution.
Other pools cover ingress, delivery, OAuth, deployment, skill, and maintenance
tasks as configured.

## `runner`

`runner.lease_ttl_seconds` and `runner.max_claims` retain the compatibility
runner lease settings. New categorized execution settings belong under
`workers.pools`.

## `auth`

The backend supports:

- `source_signing_secret` for trusted source requests;
- `capability_secret` for scoped capabilities;
- `portal_identity_secret` for portal identities;
- `connector_secret_key` plus `previous_connector_secret_keys` for encrypted
  connector-secret rotation;
- `grpc_internal_token`; and
- `replay_window_seconds`.

The checked-in values are development placeholders and must be replaced outside
local use.

## Local PostgreSQL

```bash
local-pg guard knowledge-core

env GOCACHE=/private/tmp/knowlega-gocache \
  go run ./cmd/qm-backend --config configs/qm-config.yaml
```

Set `database.url` and the shared provider/Harness settings under `qm.models`.
Obsolete nested query settings are rejected.

The shared local database listens on `127.0.0.1:55433` as user `kbcore` with
database `kbcore`. A `postgres`/`5432` DSN belongs to a different server; use
the compose DSN above or change all four connection fields together.
