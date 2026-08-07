CREATE TABLE IF NOT EXISTS environments (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  name TEXT,
  owner_actor_id TEXT,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_environments_org_updated ON environments(org_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS environment_attachments (
  scope_id TEXT PRIMARY KEY,
  environment_id TEXT NOT NULL REFERENCES environments(id),
  attached_by TEXT NOT NULL,
  attached_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_environment_attachments_environment ON environment_attachments(environment_id);
