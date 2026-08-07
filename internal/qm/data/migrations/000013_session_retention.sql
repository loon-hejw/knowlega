CREATE TABLE IF NOT EXISTS session_entries (
  session_id TEXT NOT NULL,
  seq INT NOT NULL,
  parent_seq INT,
  type TEXT NOT NULL,
  payload TEXT,
  scope_label TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY(session_id, seq)
);
CREATE TABLE IF NOT EXISTS participants (
  session_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  valid_from BIGINT NOT NULL,
  valid_to BIGINT,
  valid_from_seq INT,
  valid_to_seq INT,
  title TEXT,
  archived BOOLEAN NOT NULL DEFAULT FALSE,
  pinned BOOLEAN NOT NULL DEFAULT FALSE,
  color TEXT,
  PRIMARY KEY(session_id, principal_id)
);
ALTER TABLE participants ADD COLUMN IF NOT EXISTS valid_from_seq INT;
ALTER TABLE participants ADD COLUMN IF NOT EXISTS valid_to_seq INT;
CREATE INDEX IF NOT EXISTS session_entries_user_ts ON session_entries(created_at) WHERE type = 'user';
CREATE INDEX IF NOT EXISTS session_entries_session_created ON session_entries(session_id, created_at DESC);
