# Service API

## Lifecycle and Readiness

Start the QM backend with:

```bash
go run ./cmd/qm-backend --config configs/qm-config.yaml
```

`GET /healthz` and `GET /readyz` expose process and database readiness. They do
not wait for project knowledge compilation. `GET /v1/scope-resources` includes
the selected project's knowledge state (`empty`, `queued`, `processing`,
`ready`, or `failed`), source/page counts, queue counts, latest error, and last
successful compile time.

Each completed source is checkpointed in `.kbcore/source-manifest.json` and the
durable ingest queue. Restarting requeues interrupted work and skips unchanged
sources. Raw project-file mirrors are searchable immediately, including while
the workspace is still queued or processing.

The backend does not expose a standalone Knowledge listener. Project uploads
and attachments are compiled asynchronously by default when
`knowledge.auto_process_project_files: true`. This is independent of
`knowledge.worker`, which remains the opt-in switch for generic non-project
raw-directory scanning and may spend additional LLM tokens.

## Authentication

Health endpoints are unauthenticated. QM product routes use either a signed
source request or a scoped capability token, depending on the caller. Configure
the signing, capability, portal identity, connector encryption, replay window,
and internal gRPC secrets under `auth`; do not add a separate Knowledge API
token or expose Knowledge Agent methods directly.

## Route Groups

The following is a route map, not a replacement for request/response schemas in
the Go handlers and TypeScript API client.

| Area | Routes |
| --- | --- |
| Health | `GET /healthz`, `GET /readyz` |
| Project knowledge status | `GET /v1/scope-resources?principalId=...&scope=group:web-project-...` |
| Upload and attach | `POST /v1/files/upload`, `POST /v1/projects/{project_id}/files` |
| Retry and delete | `POST /v1/projects/{project_id}/files/{file_id}/retry`, `DELETE /v1/projects/{project_id}/files/{file_id}` |
| Files and evidence documents | `GET /v1/files`, `GET /v1/files/{file_id}/content`, `GET /v1/projects/{project_id}/knowledge/documents` |
| Project Q&A | normal QM conversation turns using the internal `knowledge` tool |

Common calls:

```bash
curl -sS http://127.0.0.1:19829/healthz

curl -sS 'http://127.0.0.1:19829/v1/scope-resources?principalId=alice&scope=group:web-project-123'

curl -sS 'http://127.0.0.1:19829/v1/projects/123/knowledge/documents?viewer=alice&path=wiki%2Foverview.md'
```

Knowledge does not expose file upload, source mutation, raw queue, or direct
project-file write routes. QM is the only product entry for files. It owns the
file artifact, blob, project membership, authorization, and deletion lifecycle;
the internal Knowledge Agent receives an immutable, read-only raw mirror and
maintains generated wiki artifacts asynchronously.

Project Q&A is part of the normal QM conversation turn. The selected outer Pi
loop receives one internal `knowledge` tool; Knowledge Core does not expose
`/query`, `/projects/query`, or a separate `/chats` runtime.

## Managed Code Webhooks

When `graph.enabled` is true, each configured registry exposes `POST
/webhooks/code/<registry-id>`. The handler verifies the provider-specific
signature/token, accepts only allowlisted repositories and configured branches,
and enqueues a restart-safe graph job. Optional semantic enrichment runs after
exact indexing and cannot replace exact facts.

## Knowledge Agent boundary

Knowlega is an internal Go interface, not a public RPC. Use the QM HTTP API and
control/runner gRPC for application integration. File creation and project
membership use `POST /v1/files/upload`, `POST /v1/projects/{project_id}/files`,
`POST /v1/projects/{project_id}/files/{file_id}/retry`, and
`DELETE /v1/projects/{project_id}/files/{file_id}`. This keeps ingest,
writeback, version archives, queue state, and cleanup on one implementation.
