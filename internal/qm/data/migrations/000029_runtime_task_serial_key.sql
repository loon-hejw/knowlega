ALTER TABLE runtime_tasks ADD COLUMN IF NOT EXISTS serial_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS runtime_tasks_running_serial_idx
  ON runtime_tasks(serial_key)
  WHERE status='running' AND serial_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS runtime_tasks_serial_pending_idx
  ON runtime_tasks(serial_key,available_at,created_at)
  WHERE status='pending' AND serial_key IS NOT NULL;
