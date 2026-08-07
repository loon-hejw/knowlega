CREATE TABLE IF NOT EXISTS knowledge_scopes (
  org_id TEXT NOT NULL,
  external_scope_id TEXT NOT NULL,
  scope_kind TEXT NOT NULL,
  project_id TEXT NOT NULL,
  project_name TEXT NOT NULL,
  root_path TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (org_id, external_scope_id, scope_kind)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_knowledge_scopes_project_id ON knowledge_scopes(project_id);
