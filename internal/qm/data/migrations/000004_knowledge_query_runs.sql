CREATE TABLE IF NOT EXISTS knowledge_query_runs (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  external_scope_id TEXT NOT NULL,
  scope_kind TEXT NOT NULL,
  question TEXT NOT NULL,
  answer TEXT NOT NULL,
  citation_paths JSONB NOT NULL,
  query_plan TEXT NOT NULL,
  can_write_back BOOLEAN NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_knowledge_query_runs_scope ON knowledge_query_runs(org_id, external_scope_id, scope_kind, created_at DESC);
