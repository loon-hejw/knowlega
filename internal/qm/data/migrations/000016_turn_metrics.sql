CREATE TABLE IF NOT EXISTS turn_metrics (
  id BIGSERIAL PRIMARY KEY,
  ts BIGINT NOT NULL,
  scope_label TEXT NOT NULL,
  session_id TEXT,
  turn_seq INT,
  run_id TEXT,
  status TEXT NOT NULL,
  total_ms INT NOT NULL,
  ttft_ms INT,
  intake_preamble_ms INT,
  dispatch_ms INT,
  provisioned BOOLEAN,
  cold_start BOOLEAN,
  model_calls INT,
  tool_calls INT,
  provision_ms INT,
  materialize_ms INT,
  creds_ms INT,
  layers_ms INT,
  compile_ms INT,
  recall_ms INT,
  exec_ms INT,
  stream_ms INT,
  lease_ms INT,
  capture_ms INT,
  ingress_ms INT,
  detect_ms INT,
  compact_ms INT,
  queue_ms INT,
  deliver_ms INT,
  slack_inflight_ms INT,
  resumed_from_seq INT,
  cache_read BIGINT,
  cache_write BIGINT,
  uncached_input BIGINT
);
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS session_id TEXT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS turn_seq INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS run_id TEXT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS ttft_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS intake_preamble_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS dispatch_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS provisioned BOOLEAN;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS cold_start BOOLEAN;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS model_calls INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS tool_calls INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS provision_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS materialize_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS creds_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS layers_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS compile_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS recall_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS exec_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS stream_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS lease_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS capture_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS ingress_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS detect_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS compact_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS queue_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS deliver_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS slack_inflight_ms INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS resumed_from_seq INT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS cache_read BIGINT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS cache_write BIGINT;
ALTER TABLE turn_metrics ADD COLUMN IF NOT EXISTS uncached_input BIGINT;
CREATE INDEX IF NOT EXISTS turn_metrics_by_ts ON turn_metrics(ts DESC);
CREATE INDEX IF NOT EXISTS turn_metrics_by_scope_ts ON turn_metrics(scope_label, ts DESC);
CREATE INDEX IF NOT EXISTS turn_metrics_by_session ON turn_metrics(session_id, ts DESC);
CREATE INDEX IF NOT EXISTS turn_metrics_by_run ON turn_metrics(run_id);
