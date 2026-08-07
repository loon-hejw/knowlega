package postgres

const BootstrapSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS projects (
  id text PRIMARY KEY,
  name text NOT NULL,
  root_path text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS scope_bindings (
  provider text NOT NULL,
  external_scope_id text NOT NULL,
  kind text NOT NULL,
  organization_id text NOT NULL DEFAULT '',
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  project_name text NOT NULL DEFAULT '',
  root_path text NOT NULL,
  status text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, external_scope_id),
  UNIQUE (provider, project_id)
);

CREATE INDEX IF NOT EXISTS scope_bindings_project_idx ON scope_bindings(project_id);

CREATE TABLE IF NOT EXISTS sources (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  path text NOT NULL,
  kind text NOT NULL,
  title text NOT NULL,
  sha256 text NOT NULL,
  immutable boolean NOT NULL DEFAULT true,
  original_path text NOT NULL DEFAULT '',
  imported_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, path)
);

CREATE TABLE IF NOT EXISTS wiki_pages (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  path text NOT NULL,
  type text NOT NULL,
  title text NOT NULL,
  body text NOT NULL,
  frontmatter jsonb NOT NULL DEFAULT '{}'::jsonb,
  sources text[] NOT NULL DEFAULT '{}',
  search_vector tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
    setweight(to_tsvector('simple', coalesce(frontmatter->>'aliases', '')), 'A') ||
    setweight(to_tsvector('simple', coalesce(body, '')), 'B')
  ) STORED,
  embedding vector,
  embedding_model text NOT NULL DEFAULT '',
  embedding_source_sha256 text NOT NULL DEFAULT '',
  embedding_updated_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, path)
);

ALTER TABLE wiki_pages ADD COLUMN IF NOT EXISTS embedding_model text NOT NULL DEFAULT '';
ALTER TABLE wiki_pages ADD COLUMN IF NOT EXISTS embedding_source_sha256 text NOT NULL DEFAULT '';
ALTER TABLE wiki_pages ADD COLUMN IF NOT EXISTS embedding_updated_at timestamptz;

CREATE INDEX IF NOT EXISTS wiki_pages_search_idx ON wiki_pages USING gin(search_vector);
CREATE INDEX IF NOT EXISTS wiki_pages_frontmatter_idx ON wiki_pages USING gin(frontmatter);

CREATE TABLE IF NOT EXISTS wiki_page_versions (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  page_id text NOT NULL,
  path text NOT NULL,
  body text NOT NULL,
  frontmatter jsonb NOT NULL DEFAULT '{}'::jsonb,
  sources text[] NOT NULL DEFAULT '{}',
  reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS wiki_page_versions_page_idx ON wiki_page_versions(project_id, page_id, created_at DESC);
CREATE INDEX IF NOT EXISTS wiki_page_versions_path_idx ON wiki_page_versions(project_id, path, created_at DESC);

CREATE TABLE IF NOT EXISTS source_manifest (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  original_path text NOT NULL,
  pipeline_version integer NOT NULL DEFAULT 0,
  sha256 text NOT NULL,
  raw_path text NOT NULL,
  archive_path text NOT NULL DEFAULT '',
  original_raw_path text NOT NULL DEFAULT '',
  content_path text NOT NULL DEFAULT '',
  original_sha256 text NOT NULL DEFAULT '',
  content_sha256 text NOT NULL DEFAULT '',
  title text NOT NULL,
  files text[] NOT NULL DEFAULT '{}',
  generation_contract_sha256 text NOT NULL DEFAULT '',
  new_page_budget integer NOT NULL DEFAULT 0,
  new_page_count integer NOT NULL DEFAULT 0,
  created_pages text[] NOT NULL DEFAULT '{}',
  review_count integer NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, original_path)
);

ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS archive_path text NOT NULL DEFAULT '';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS pipeline_version integer NOT NULL DEFAULT 0;
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS original_raw_path text NOT NULL DEFAULT '';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS content_path text NOT NULL DEFAULT '';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS original_sha256 text NOT NULL DEFAULT '';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS generation_contract_sha256 text NOT NULL DEFAULT '';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS new_page_budget integer NOT NULL DEFAULT 0;
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS new_page_count integer NOT NULL DEFAULT 0;
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS created_pages text[] NOT NULL DEFAULT '{}';
ALTER TABLE source_manifest ADD COLUMN IF NOT EXISTS content_sha256 text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS source_manifest_sha_idx ON source_manifest(project_id, sha256);
CREATE INDEX IF NOT EXISTS source_manifest_archive_idx ON source_manifest(project_id, archive_path);

CREATE TABLE IF NOT EXISTS code_repos (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  repo_path text NOT NULL,
  head_commit text NOT NULL DEFAULT '',
  indexed_commit text NOT NULL DEFAULT '',
  primary_graph_source text NOT NULL DEFAULT 'gitnexus',
  stale boolean NOT NULL DEFAULT false,
  synced_at timestamptz,
  UNIQUE(project_id, repo_path)
);

CREATE TABLE IF NOT EXISTS graph_nodes (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  repo_id text REFERENCES code_repos(id) ON DELETE CASCADE,
  domain text NOT NULL DEFAULT 'code',
  scope_id text NOT NULL DEFAULT '',
  kind text NOT NULL,
  label text NOT NULL,
  source_ref text NOT NULL DEFAULT '',
  props jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS graph_edges (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  repo_id text REFERENCES code_repos(id) ON DELETE CASCADE,
  domain text NOT NULL DEFAULT 'code',
  scope_id text NOT NULL DEFAULT '',
  src_id text NOT NULL,
  dst_id text NOT NULL,
  relation text NOT NULL,
  confidence text NOT NULL DEFAULT 'INFERRED',
  confidence_score double precision NOT NULL DEFAULT 0.75,
  weight double precision NOT NULL DEFAULT 1,
  evidence text[] NOT NULL DEFAULT '{}',
  props jsonb NOT NULL DEFAULT '{}'::jsonb
);

ALTER TABLE graph_nodes ADD COLUMN IF NOT EXISTS domain text NOT NULL DEFAULT 'code';
ALTER TABLE graph_nodes ADD COLUMN IF NOT EXISTS scope_id text NOT NULL DEFAULT '';
ALTER TABLE graph_edges ADD COLUMN IF NOT EXISTS domain text NOT NULL DEFAULT 'code';
ALTER TABLE graph_edges ADD COLUMN IF NOT EXISTS scope_id text NOT NULL DEFAULT '';
ALTER TABLE graph_edges ADD COLUMN IF NOT EXISTS confidence_score double precision NOT NULL DEFAULT 0.75;
ALTER TABLE graph_edges ADD COLUMN IF NOT EXISTS evidence text[] NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS graph_edges_src_idx ON graph_edges(project_id, src_id, relation);
CREATE INDEX IF NOT EXISTS graph_edges_dst_idx ON graph_edges(project_id, dst_id, relation);
CREATE INDEX IF NOT EXISTS graph_nodes_kind_idx ON graph_nodes(project_id, kind);
CREATE INDEX IF NOT EXISTS graph_nodes_domain_scope_idx ON graph_nodes(project_id, domain, scope_id, kind);
CREATE INDEX IF NOT EXISTS graph_edges_domain_scope_idx ON graph_edges(project_id, domain, scope_id, relation);
CREATE INDEX IF NOT EXISTS graph_edges_confidence_score_idx ON graph_edges(project_id, confidence_score DESC);

CREATE TABLE IF NOT EXISTS ingest_jobs (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  kind text NOT NULL,
  status text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS review_items (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  type text NOT NULL,
  title text NOT NULL,
  description text NOT NULL,
  severity text NOT NULL DEFAULT 'info',
  status text NOT NULL DEFAULT 'unresolved',
  affected_pages text[] NOT NULL DEFAULT '{}',
  props jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz
);

CREATE TABLE IF NOT EXISTS query_logs (
  id text PRIMARY KEY,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  query text NOT NULL,
  mode text NOT NULL,
  result_count integer NOT NULL DEFAULT 0,
  answer_payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE query_logs ADD COLUMN IF NOT EXISTS answer_payload jsonb NOT NULL DEFAULT '{}'::jsonb;
`
