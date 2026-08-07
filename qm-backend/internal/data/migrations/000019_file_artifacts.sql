-- Shared with qm/src/files/postgres-file-artifact-store.ts. Metadata is
-- control-plane state; blob bytes continue to live in the configured durable
-- byte store.
CREATE TABLE IF NOT EXISTS file_artifacts(
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL DEFAULT 'file',
  owner_scope_id   TEXT NOT NULL,
  path             TEXT NOT NULL,
  name             TEXT NOT NULL,
  mimetype         TEXT NOT NULL,
  size_bytes       BIGINT NOT NULL,
  blob_key         TEXT,
  sha256           TEXT,
  direction        TEXT NOT NULL,
  created_by       TEXT NOT NULL,
  created_in_scope TEXT,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  enabled          BOOLEAN NOT NULL DEFAULT TRUE,
  source           TEXT NOT NULL DEFAULT 'live'
);

CREATE INDEX IF NOT EXISTS file_artifacts_owner_created
  ON file_artifacts (owner_scope_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS file_artifacts_owner_path
  ON file_artifacts (owner_scope_id, path);
CREATE INDEX IF NOT EXISTS file_artifacts_scope_created
  ON file_artifacts (created_in_scope, created_at DESC, id DESC) WHERE enabled = TRUE;
