CREATE TABLE IF NOT EXISTS runtime_tasks(
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL,
  payload_version  INTEGER NOT NULL DEFAULT 1,
  payload          JSONB NOT NULL,
  scope_id         TEXT,
  actor_id         TEXT,
  idempotency_key  TEXT,
  serial_key       TEXT,
  status           TEXT NOT NULL DEFAULT 'pending'
                   CHECK(status IN ('pending','running','succeeded','failed','cancelled','dead')),
  priority         INTEGER NOT NULL DEFAULT 0,
  available_at     BIGINT NOT NULL,
  attempts         INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 3,
  lease_token      TEXT,
  lease_owner      TEXT,
  lease_expires_at BIGINT,
  cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
  result           JSONB,
  last_error       TEXT,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  started_at       BIGINT,
  finished_at      BIGINT
);

CREATE UNIQUE INDEX IF NOT EXISTS runtime_tasks_kind_idempotency_idx
  ON runtime_tasks(kind,idempotency_key)
  WHERE idempotency_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS runtime_tasks_claim_idx
  ON runtime_tasks(kind,status,available_at,priority DESC,created_at)
  WHERE status='pending';

CREATE INDEX IF NOT EXISTS runtime_tasks_expired_lease_idx
  ON runtime_tasks(lease_expires_at)
  WHERE status='running';

CREATE UNIQUE INDEX IF NOT EXISTS runtime_tasks_running_serial_idx
  ON runtime_tasks(serial_key)
  WHERE status='running' AND serial_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS runtime_task_events(
  task_id     TEXT NOT NULL REFERENCES runtime_tasks(id) ON DELETE CASCADE,
  sequence    BIGINT NOT NULL,
  type        TEXT NOT NULL,
  payload     JSONB NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY(task_id,sequence)
);
