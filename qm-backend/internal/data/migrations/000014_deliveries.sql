CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  destination JSONB NOT NULL,
  text TEXT NOT NULL,
  attachments JSONB,
  provenance JSONB,
  created_at BIGINT NOT NULL,
  delivered_at BIGINT,
  recipient_thread_ref TEXT,
  shadow BOOLEAN NOT NULL DEFAULT FALSE,
  deliver_latency_ms INT,
  slack_api_ms INT,
  claim_expires_at BIGINT
);
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS attachments JSONB;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS provenance JSONB;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS recipient_thread_ref TEXT;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS shadow BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS deliver_latency_ms INT;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS slack_api_ms INT;
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS claim_expires_at BIGINT;
CREATE INDEX IF NOT EXISTS idx_deliveries_pending
  ON deliveries ((destination->>'type'), created_at) WHERE delivered_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_deliveries_recipient_thread
  ON deliveries (recipient_thread_ref, created_at) WHERE recipient_thread_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_deliveries_shadow
  ON deliveries (created_at) WHERE shadow AND delivered_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_deliveries_source_session
  ON deliveries ((provenance->>'sourceSessionId'), created_at) WHERE provenance IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_deliveries_source_thread
  ON deliveries ((provenance->>'sourceThreadRef'), created_at) WHERE provenance IS NOT NULL;
