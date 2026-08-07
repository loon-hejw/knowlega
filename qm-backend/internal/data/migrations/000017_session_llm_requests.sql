CREATE TABLE IF NOT EXISTS session_llm_requests (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  turn_seq INT,
  step INT NOT NULL,
  model TEXT NOT NULL,
  scope_label TEXT NOT NULL,
  request TEXT NOT NULL,
  truncated BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  ttft_ms INT,
  duration_ms INT,
  step_gap_ms INT,
  tool_wall_json TEXT,
  usage_json TEXT,
  transport_json TEXT,
  gap_phases_json TEXT
);
CREATE INDEX IF NOT EXISTS session_llm_requests_by_session
  ON session_llm_requests(session_id, created_at, step);
