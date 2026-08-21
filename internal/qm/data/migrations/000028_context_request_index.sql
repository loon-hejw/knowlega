DROP INDEX IF EXISTS context_requests_pending_idx;

CREATE INDEX context_requests_pending_idx
  ON context_requests((json->>'source'),(json->>'status'))
  WHERE json->>'status'='pending';
