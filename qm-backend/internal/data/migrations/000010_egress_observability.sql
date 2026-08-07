CREATE TABLE IF NOT EXISTS credential_usage (
  id BIGSERIAL PRIMARY KEY,
  ts BIGINT NOT NULL,
  slug TEXT NOT NULL,
  host TEXT NOT NULL,
  status TEXT NOT NULL,
  upstream_status INT,
  scope_label TEXT NOT NULL,
  principal_id TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS credential_usage_by_ts ON credential_usage(ts DESC);
CREATE INDEX IF NOT EXISTS credential_usage_by_scope_ts ON credential_usage(scope_label, ts DESC);

CREATE TABLE IF NOT EXISTS egress_events (
  id BIGSERIAL PRIMARY KEY,
  ts BIGINT NOT NULL,
  source TEXT NOT NULL,
  host TEXT NOT NULL,
  allowed BOOLEAN NOT NULL,
  scope_label TEXT NOT NULL,
  port INT,
  verdict TEXT,
  via TEXT,
  peer_ip TEXT,
  principal_id TEXT
);
CREATE INDEX IF NOT EXISTS egress_events_by_ts ON egress_events(ts DESC);
CREATE INDEX IF NOT EXISTS egress_events_by_scope_ts ON egress_events(scope_label, ts DESC);
