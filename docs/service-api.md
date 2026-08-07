# Service API

## Lifecycle and Readiness

Start the QM backend with:

```bash
go run ./cmd/qm-backend --config qm-backend/configs/config.yaml
```

`GET /healthz` and `GET /readyz` expose backend readiness. Knowlega workspace
status, queue progress, lint, review, and maintenance are internal Agent state;
QM handlers invoke them after successful product writes.

Bootstrap checkpoints each completed source in
`.kbcore/source-manifest.json`. Restarting resumes from durable state and skips
unchanged sources. `wait-ready` is available for scripts that must block until
the complete workspace is ready.

The backend does not expose a standalone Knowledge listener. Queue consumption
is an explicit QM/Agent maintenance operation so starting the server does not
unexpectedly spend LLM tokens.

## Authentication

The service is local-first. Configure `server.api_token` to require:

```http
Authorization: Bearer <token>
```

Non-loopback requests require protection. Set `server.api_require_token: true`
to require the token for loopback clients as well. Keep the default
`127.0.0.1` binding when remote exposure is unnecessary.

## Route Groups

The following is a route map, not a replacement for request/response schemas in
the Go handlers and TypeScript API client.

| Area | Routes |
| --- | --- |
| Health/workspace | `GET /health`, `GET /workspace/status`, `POST /workspace/maintain`, `GET /workspace/jobs`, `GET /workspace/jobs/{id}` |
| Project files | `GET /projects/files`, `GET/PUT /projects/files/content`, `POST /projects/search` |
| Sources | `GET /projects/sources`, `POST /projects/sources/upload`, `POST /projects/sources/rescan`, `POST /projects/sources/delete`, layout-migration GET/POST routes |
| Legacy/project operations | `POST /projects/init`, `POST /projects/ingest`, `GET /projects/query`, `GET /projects/lint` |
| Queue and compile | `POST /sources/queue`, `POST /sources/scan`, `GET /queue/tasks`, `POST /queue/run`, `POST /wiki/validate` |
| Query/chat | `POST /query`, chat collection/item/message/run/event/cancel routes under `/chats` |
| Review | `GET /reviews`, resolve/bulk/action/sweep routes, `POST /wiki/review` |
| Research | `GET/POST /research/jobs`, `GET /research/jobs/{id}` |
| Graph | project graph/query/node/insights/repositories/jobs routes, `POST /code/import-graphify` |
| Persistence | `POST /wiki/sync-pg` |
| Webhooks | `POST /webhooks/code/{registry}` |

Common calls:

```bash
curl -sS http://127.0.0.1:19829/health

curl -sS -X POST http://127.0.0.1:19829/sources/queue \
  -H 'Content-Type: application/json' \
  -d '{"source_path":"/path/to/new-file.md","title":"New File"}'

curl -sS -X POST http://127.0.0.1:19829/query \
  -H 'Content-Type: application/json' \
  -d '{"q":"How does source archiving work?","agent":"llm","save_title":"Source archiving"}'

curl -sS 'http://127.0.0.1:19829/reviews?status=open'
```

Source deletion supports a dry-run request and must preserve immutable/raw and
wiki provenance rules. File writes are restricted to safe project-relative
paths.

## Managed Code Webhooks

When `graph.enabled` is true, each configured registry exposes `POST
/webhooks/code/<registry-id>`. The handler verifies the provider-specific
signature/token, accepts only allowlisted repositories and configured branches,
and enqueues a restart-safe graph job. Optional semantic enrichment runs after
exact indexing and cannot replace exact facts.

## Knowledge Agent boundary

Knowlega is an internal Go interface, not a public RPC. Use the QM HTTP API and
control/runner gRPC for application integration. This keeps query, ingest,
writeback, version archives, queue state, and cleanup on one implementation.
