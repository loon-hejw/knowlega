-- Shared with qm/src/resolution/config-store.ts. soul_history is retained for
-- Node's revision maintenance while Go initially serves the current read view.
CREATE TABLE IF NOT EXISTS soul_configs(
  id   TEXT PRIMARY KEY,
  json JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS soul_history(
  id   TEXT PRIMARY KEY,
  json JSONB NOT NULL
);
