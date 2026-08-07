CREATE TABLE IF NOT EXISTS error_events (
  id BIGSERIAL PRIMARY KEY,
  ts BIGINT NOT NULL,
  scope_label TEXT NOT NULL,
  category TEXT NOT NULL,
  code TEXT NOT NULL,
  message TEXT NOT NULL,
  session_id TEXT
);
CREATE INDEX IF NOT EXISTS error_events_by_ts ON error_events(ts DESC);
CREATE INDEX IF NOT EXISTS error_events_by_scope_ts ON error_events(scope_label, ts DESC);
CREATE INDEX IF NOT EXISTS error_events_by_session_ts ON error_events(session_id, ts DESC);
