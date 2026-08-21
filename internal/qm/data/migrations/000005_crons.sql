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
-- CREATE TABLE IF NOT EXISTS does not reconcile an existing Node-era table.
-- Add the Go projection columns idempotently before creating its partial index.
ALTER TABLE crons ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE crons ADD COLUMN IF NOT EXISTS archived BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE crons ADD COLUMN IF NOT EXISTS next_fire_at BIGINT;
ALTER TABLE crons ADD COLUMN IF NOT EXISTS last_fired_at BIGINT;
ALTER TABLE crons ADD COLUMN IF NOT EXISTS created_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE crons ADD COLUMN IF NOT EXISTS updated_at BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_crons_due ON crons(next_fire_at) WHERE enabled AND NOT archived;
