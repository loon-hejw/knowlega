# QM Backend

Kratos control plane for QM. It preserves the existing `/v1` HTTP contract while
moving durable control-plane state from the Node core to Go. Node remains the
agent runner and surface adapter until those boundaries are migrated.

Run locally with the shared QM YAML (including PostgreSQL and model settings):

```bash
go run ./cmd/qm-backend --config configs/qm-config.yaml
```

Generate the checked-in gRPC bindings with `make generate`; run the normal suite
with `make test`. PostgreSQL integration tests are enabled by setting
`QM_BACKEND_TEST_DATABASE_URL`.

## Project files and Knowledge

QM is the authoritative file system for product workflows. A project file is a
membership between a QM project and a QM file artifact; one file may be attached
to more than one project. Supported text, Markdown, PDF, and DOCX files are
queued into that project's Knowledge workspace after upload or attachment.
`knowledge.auto_process_project_files` defaults to `true`, so this queue is
consumed without an administrator calling a maintenance endpoint. The separate
`knowledge.worker` switch only enables generic scans for non-project scopes.

Knowledge keeps a read-only raw mirror keyed by QM file ID and SHA-256, plus the
generated Markdown wiki, source manifest, reviews, and version history. Its
standalone upload, source mutation, raw queue, and direct project-file routes are
not exposed. Removing a project membership cleans that project's generated
knowledge. Removing the final membership also deletes the QM file record and an
unreferenced local blob.

Project membership state moves through `queued`, `processing`, and `ready` or
`failed`. Worker startup requeues an interrupted `processing` task, and repeated
maintenance remains idempotent through the source hash and manifest.

QM routes:

- `POST /v1/files/upload` creates the authoritative artifact and optionally a
  project membership when the upload scope is a project.
- `POST /v1/projects/{project_id}/files` attaches an existing visible QM file.
- `POST /v1/projects/{project_id}/files/{file_id}/retry` queues a failed file
  again.
- `DELETE /v1/projects/{project_id}/files/{file_id}` removes the project
  membership and applies the lifecycle rules above.

### Importing a legacy Markdown knowledge project

Use the backend's one-shot import mode when an existing `purpose.md` / `schema.md`
/ `raw/` / `wiki/` project predates QM file ownership:

```bash
go run ./cmd/qm-backend \
  --config /path/to/the-running-qm-config.yaml \
  --import-legacy-knowledge /path/to/legacy-project \
  --import-project-name "Project name" \
  --import-project-owner principal-id
```

The command creates or reuses the named QM project, copies the legacy project
into its managed Knowledge scope, registers every current manifest source as a
content-addressed QM file, stores project memberships as `ready`, adds QM source
provenance to the Markdown manifest, and performs a full PostgreSQL wiki sync.
The source directory is not moved or modified. Stable artifact IDs and an import
marker make retries idempotent; the command refuses to overlay a non-empty
target scope or a scope imported from a different source.

See [Node Core Migration](docs/node-core-migration.md) for the deployment and
route-cutover procedure.
