-- Shared with qm/src/memory/postgres-memory-service.ts. Each row is a durable
-- revision; the current memory head is the greatest seq for its scope.
CREATE TABLE IF NOT EXISTS memory_revisions(
  id        BIGSERIAL PRIMARY KEY,
  scope_id  TEXT   NOT NULL,
  seq       BIGINT NOT NULL,
  op        TEXT   NOT NULL,
  body      TEXT   NOT NULL,
  author    TEXT,
  at        BIGINT NOT NULL,
  UNIQUE (scope_id, seq)
);

CREATE INDEX IF NOT EXISTS memory_revisions_by_scope
  ON memory_revisions(scope_id, seq DESC);
