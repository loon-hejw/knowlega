CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW());
CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, json JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS durable_map_versions (tbl TEXT PRIMARY KEY, v BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS deactivated_principals (id TEXT PRIMARY KEY, json JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS source_auth_replay (event_id TEXT PRIMARY KEY, expires_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS source_auth_replay_expires_at ON source_auth_replay (expires_at);
CREATE TABLE IF NOT EXISTS directory_members (
  org_id TEXT NOT NULL, principal_id TEXT NOT NULL, display_name TEXT NOT NULL, display_name_lc TEXT NOT NULL,
  type TEXT NOT NULL, slack_id TEXT, PRIMARY KEY (org_id, principal_id)
);
CREATE INDEX IF NOT EXISTS directory_members_name ON directory_members (org_id, display_name_lc);
CREATE INDEX IF NOT EXISTS directory_members_lower_pid ON directory_members (org_id, lower(principal_id));
CREATE TABLE IF NOT EXISTS directory_channels (
  org_id TEXT NOT NULL, channel_id TEXT NOT NULL, name TEXT NOT NULL, name_lc TEXT NOT NULL,
  is_private BOOLEAN NOT NULL DEFAULT FALSE, PRIMARY KEY (org_id, channel_id)
);
CREATE TABLE IF NOT EXISTS directory_channel_members (
  org_id TEXT NOT NULL, channel_id TEXT NOT NULL, principal_id TEXT NOT NULL, PRIMARY KEY (org_id, channel_id, principal_id)
);
CREATE TABLE IF NOT EXISTS directory_group_members (
  org_id TEXT NOT NULL, group_id TEXT NOT NULL, principal_id TEXT NOT NULL, PRIMARY KEY (org_id, group_id, principal_id)
);
CREATE INDEX IF NOT EXISTS directory_group_members_principal ON directory_group_members (org_id, principal_id, group_id);
CREATE TABLE IF NOT EXISTS directory_sync (
  org_id TEXT PRIMARY KEY, members_hash TEXT, channels_hash TEXT, groups_hash TEXT,
  channel_members_synced BOOLEAN NOT NULL DEFAULT FALSE, updated_at BIGINT NOT NULL,
  members_synced_at BIGINT, channels_synced_at BIGINT, groups_synced_at BIGINT
);
CREATE TABLE IF NOT EXISTS directory_meta (org_id TEXT PRIMARY KEY, workspace_url TEXT, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS acl_grants (
  owner_scope_id TEXT NOT NULL, path TEXT NOT NULL, grantee_scope_id TEXT NOT NULL,
  permission TEXT NOT NULL, granted_by TEXT NOT NULL,
  PRIMARY KEY (owner_scope_id, path, grantee_scope_id, permission)
);
CREATE TABLE IF NOT EXISTS admin_grants (
  principal_id TEXT NOT NULL, scope_id TEXT NOT NULL, role TEXT NOT NULL, granted_by TEXT, created_at BIGINT,
  PRIMARY KEY (principal_id, scope_id, role)
);
CREATE TABLE IF NOT EXISTS audit_log (
  id BIGSERIAL PRIMARY KEY, at BIGINT NOT NULL, principal_id TEXT NOT NULL, action TEXT NOT NULL,
  resource TEXT NOT NULL, scope_label TEXT NOT NULL, status TEXT, detail TEXT, idempotency_key TEXT
);
CREATE INDEX IF NOT EXISTS audit_log_by_at ON audit_log (at DESC);
CREATE INDEX IF NOT EXISTS audit_log_by_scope_at ON audit_log (scope_label, at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS audit_log_by_idempotency_key ON audit_log (idempotency_key) WHERE idempotency_key IS NOT NULL;
