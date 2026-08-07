CREATE TABLE IF NOT EXISTS channel_messages (
  org_id TEXT NOT NULL,
  container TEXT NOT NULL,
  ts TEXT NOT NULL,
  sub TEXT,
  author_id TEXT,
  author_name TEXT,
  text TEXT NOT NULL DEFAULT '',
  mentions JSONB,
  self BOOLEAN NOT NULL DEFAULT FALSE,
  bot BOOLEAN NOT NULL DEFAULT FALSE,
  mentions_self BOOLEAN NOT NULL DEFAULT FALSE,
  edited_at BIGINT,
  deleted BOOLEAN NOT NULL DEFAULT FALSE,
  handled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  PRIMARY KEY(org_id, container, ts)
);
CREATE INDEX IF NOT EXISTS channel_messages_by_container ON channel_messages(org_id, container, ts);
CREATE INDEX IF NOT EXISTS channel_messages_live_by_container ON channel_messages(org_id, container) WHERE deleted = FALSE;
CREATE INDEX IF NOT EXISTS channel_messages_by_sub ON channel_messages(org_id, container, sub, ts);
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS mentions JSONB;
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS bot BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS mentions_self BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS handled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE channel_messages ADD COLUMN IF NOT EXISTS tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(text, ''))) STORED;
CREATE INDEX IF NOT EXISTS channel_messages_tsv ON channel_messages USING GIN(tsv);
CREATE TABLE IF NOT EXISTS channel_state (
  org_id TEXT NOT NULL,
  container TEXT NOT NULL,
  last_ts TEXT,
  oldest_ts TEXT,
  name TEXT,
  kind TEXT,
  members JSONB NOT NULL DEFAULT '[]'::jsonb,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY(org_id, container)
);
ALTER TABLE channel_state ADD COLUMN IF NOT EXISTS oldest_ts TEXT;
ALTER TABLE channel_state ADD COLUMN IF NOT EXISTS kind TEXT;
CREATE TABLE IF NOT EXISTS channel_files (
  org_id TEXT NOT NULL,
  container TEXT NOT NULL,
  ts TEXT NOT NULL,
  file_id TEXT NOT NULL,
  name TEXT,
  mimetype TEXT,
  created_at BIGINT NOT NULL,
  PRIMARY KEY(org_id, container, ts, file_id)
);
