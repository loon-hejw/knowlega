-- Shared with qm/src/deployment/deployment-layer-store.ts. This table is a
-- one-row DurableMap (id = current); Node still owns validation and runtime
-- materialization while Kratos owns the atomic PostgreSQL boundary.
CREATE TABLE IF NOT EXISTS deployment_layer(
  id TEXT PRIMARY KEY,
  json JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS durable_map_versions(
  tbl TEXT PRIMARY KEY,
  v BIGINT NOT NULL
);
