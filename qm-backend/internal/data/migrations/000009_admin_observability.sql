CREATE TABLE IF NOT EXISTS ambient_judgments (
  id BIGSERIAL PRIMARY KEY,
  org_id TEXT NOT NULL,
  surface TEXT NOT NULL,
  container TEXT NOT NULL,
  decision TEXT NOT NULL,
  reason TEXT,
  asked_by TEXT,
  prompt TEXT,
  model TEXT,
  latency_ms INT,
  ts_from TEXT,
  ts_to TEXT,
  created_at BIGINT NOT NULL
);
ALTER TABLE ambient_judgments ADD COLUMN IF NOT EXISTS asked_by TEXT;
CREATE INDEX IF NOT EXISTS ambient_judgments_org_container_created
  ON ambient_judgments(org_id, container, created_at DESC);
CREATE INDEX IF NOT EXISTS ambient_judgments_org_created
  ON ambient_judgments(org_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS ambient_judgments_org_decision
  ON ambient_judgments(org_id, decision);

CREATE TABLE IF NOT EXISTS ack_emoji_picks (
  id BIGSERIAL PRIMARY KEY,
  org_id TEXT NOT NULL,
  surface TEXT NOT NULL,
  channel TEXT NOT NULL,
  ts TEXT NOT NULL,
  outcome TEXT NOT NULL,
  picked TEXT,
  icon TEXT,
  message TEXT,
  candidates TEXT,
  model TEXT,
  latency_ms INT,
  created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS ack_emoji_picks_org_channel_created
  ON ack_emoji_picks(org_id, channel, created_at DESC);
CREATE INDEX IF NOT EXISTS ack_emoji_picks_org_created
  ON ack_emoji_picks(org_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS ack_emoji_picks_org_outcome
  ON ack_emoji_picks(org_id, outcome);
