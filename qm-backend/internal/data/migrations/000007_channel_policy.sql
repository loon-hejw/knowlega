CREATE TABLE IF NOT EXISTS channel_policy (
  org_id TEXT NOT NULL,
  container TEXT NOT NULL,
  orders TEXT NOT NULL DEFAULT '',
  bots JSONB NOT NULL DEFAULT '{}'::jsonb,
  ambient_enabled BOOLEAN,
  set_by TEXT,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY(org_id, container)
);
CREATE TABLE IF NOT EXISTS channel_policy_history (
  id BIGSERIAL PRIMARY KEY,
  org_id TEXT NOT NULL,
  container TEXT NOT NULL,
  orders TEXT NOT NULL,
  bots JSONB,
  ambient_enabled BOOLEAN,
  set_by TEXT,
  session_id TEXT,
  created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS channel_policy_history_container ON channel_policy_history(org_id, container, id DESC);
