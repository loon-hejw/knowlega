ALTER TABLE runs ADD COLUMN IF NOT EXISTS runtime_owner TEXT NOT NULL DEFAULT 'node'
  CHECK(runtime_owner IN ('node','go'));

CREATE INDEX IF NOT EXISTS runs_node_claim_idx
  ON runs(status,created_at)
  WHERE status='pending' AND runtime_owner='node';
