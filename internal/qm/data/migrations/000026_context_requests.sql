CREATE TABLE IF NOT EXISTS context_requests(
  id   TEXT PRIMARY KEY,
  json JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS context_request_tokens(
  request_id TEXT PRIMARY KEY REFERENCES context_requests(id) ON DELETE CASCADE,
  token_enc  TEXT NOT NULL,
  created_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS context_requests_pending_idx
  ON context_requests((json->>'source'),(json->>'status'))
  WHERE json->>'status'='pending';
