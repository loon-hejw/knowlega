-- Shared with qm/src/persistence/durable-map.ts and the skill stores. The
-- JSON documents remain Node-compatible durable-map records.
CREATE TABLE IF NOT EXISTS skills(
  id   TEXT PRIMARY KEY,
  json JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_packs(
  id   TEXT PRIMARY KEY,
  json JSONB NOT NULL
);
