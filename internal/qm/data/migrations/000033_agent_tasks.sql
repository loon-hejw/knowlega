CREATE TABLE IF NOT EXISTS tasks(
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  origin_run_id TEXT NOT NULL,
  title TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'in_progress', 'completed', 'skipped', 'failed')),
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_session_open
  ON tasks(session_id, created_at, id) WHERE status IN ('pending', 'in_progress');

CREATE INDEX IF NOT EXISTS idx_tasks_origin_run_open
  ON tasks(origin_run_id, created_at, id) WHERE status IN ('pending', 'in_progress');

CREATE INDEX IF NOT EXISTS idx_tasks_origin_run
  ON tasks(origin_run_id, created_at, id);

CREATE TABLE IF NOT EXISTS task_events(
  id BIGSERIAL PRIMARY KEY,
  task_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN ('created', 'status_changed')),
  from_status TEXT,
  to_status TEXT NOT NULL,
  created_at BIGINT NOT NULL
);

DO $$ BEGIN
  ALTER TABLE tasks ADD CONSTRAINT tasks_session_id_cascade_fkey
    FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE NOT VALID;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

DO $$ BEGIN
  ALTER TABLE task_events DROP CONSTRAINT IF EXISTS task_events_task_id_fkey;
EXCEPTION WHEN undefined_table THEN NULL; END $$;

DO $$ BEGIN
  ALTER TABLE task_events ADD CONSTRAINT task_events_task_id_cascade_fkey
    FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE NOT VALID;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, id);
