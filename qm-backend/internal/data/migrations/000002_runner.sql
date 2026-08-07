CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  status TEXT NOT NULL,
  request TEXT NOT NULL,
  result TEXT,
  idempotency_key TEXT UNIQUE,
  attempts INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  lease_token TEXT,
  lease_expires_at BIGINT,
  worker_id TEXT,
  created_at BIGINT NOT NULL,
  started_at BIGINT,
  finished_at BIGINT
);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS delivery_state TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS error_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS completion_key TEXT;
CREATE INDEX IF NOT EXISTS idx_runs_status_created ON runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_session_active_created ON runs(session_id, created_at DESC) WHERE status IN ('pending','running');
UPDATE runs SET status='pending',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL
WHERE status='running' AND id IN (
  SELECT id FROM (
    SELECT id,row_number() OVER (PARTITION BY session_id ORDER BY started_at ASC NULLS LAST,id) AS position
    FROM runs WHERE status='running'
  ) duplicates WHERE position > 1
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_one_running_per_session ON runs(session_id) WHERE status='running';

CREATE TABLE IF NOT EXISTS run_signals (
  id BIGSERIAL PRIMARY KEY,
  run_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  text TEXT,
  payload JSONB,
  created_at BIGINT NOT NULL,
  consumed_at BIGINT
);
CREATE INDEX IF NOT EXISTS idx_run_signals_pending ON run_signals(run_id) WHERE consumed_at IS NULL;

CREATE TABLE IF NOT EXISTS run_activity (
  id BIGSERIAL PRIMARY KEY,
  run_id TEXT NOT NULL,
  seq BIGINT NOT NULL,
  parent_seq BIGINT,
  type TEXT NOT NULL,
  payload JSONB NOT NULL,
  created_at BIGINT NOT NULL
);
ALTER TABLE run_activity ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
CREATE INDEX IF NOT EXISTS idx_run_activity_run ON run_activity(run_id, id);
CREATE INDEX IF NOT EXISTS idx_run_activity_created ON run_activity(created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_activity_idempotency ON run_activity(run_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
