CREATE TABLE IF NOT EXISTS agent_harness_state(
  session_id TEXT PRIMARY KEY,
  harness_id TEXT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS agent_harness_state_harness_idx
  ON agent_harness_state(harness_id,updated_at);
