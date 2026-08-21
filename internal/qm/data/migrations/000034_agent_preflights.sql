CREATE TABLE IF NOT EXISTS agent_preflights(
  run_id TEXT PRIMARY KEY,
  route TEXT NOT NULL CHECK (route IN ('pi', 'knowledge')),
  result JSONB,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
