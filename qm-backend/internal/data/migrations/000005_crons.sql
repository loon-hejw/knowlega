CREATE TABLE IF NOT EXISTS crons (
  id TEXT PRIMARY KEY,
  json JSONB NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  archived BOOLEAN NOT NULL DEFAULT FALSE,
  next_fire_at BIGINT,
  last_fired_at BIGINT,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_crons_due ON crons(next_fire_at) WHERE enabled AND NOT archived;
