CREATE TABLE IF NOT EXISTS project_file_memberships(
  project_id            TEXT NOT NULL,
  project_scope_id      TEXT NOT NULL,
  file_id               TEXT NOT NULL REFERENCES file_artifacts(id) ON DELETE CASCADE,
  knowledge_project_id  TEXT NOT NULL,
  source_sha256         TEXT NOT NULL,
  raw_path              TEXT,
  queue_task_id         TEXT,
  status                TEXT NOT NULL,
  generated_page_count  INTEGER NOT NULL DEFAULT 0,
  last_error            TEXT,
  created_at            BIGINT NOT NULL,
  updated_at            BIGINT NOT NULL,
  PRIMARY KEY(project_id,file_id)
);

CREATE INDEX IF NOT EXISTS project_file_memberships_file_idx
  ON project_file_memberships(file_id,project_id);

CREATE INDEX IF NOT EXISTS project_file_memberships_status_idx
  ON project_file_memberships(status,updated_at);
