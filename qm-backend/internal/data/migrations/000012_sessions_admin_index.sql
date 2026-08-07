CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  thread_ref TEXT UNIQUE NOT NULL,
  created_at BIGINT NOT NULL,
  title TEXT,
  channel_name TEXT
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS title TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS channel_name TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS surface TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS last_activity BIGINT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS messages INT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS turns INT;
CREATE INDEX IF NOT EXISTS sessions_by_scope ON sessions(scope_id, created_at DESC);
CREATE INDEX IF NOT EXISTS sessions_by_activity ON sessions((COALESCE(last_activity, created_at)) DESC, id DESC);
CREATE INDEX IF NOT EXISTS sessions_by_scope_activity ON sessions(scope_id, (COALESCE(last_activity, created_at)) DESC, id DESC);
