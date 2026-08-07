ALTER TABLE sessions ADD COLUMN IF NOT EXISTS forked_from_session_id TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS forked_from_title TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_boundary_seq INT;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sessions_fork_provenance_pair') THEN
    ALTER TABLE sessions ADD CONSTRAINT sessions_fork_provenance_pair
    CHECK ((forked_from_session_id IS NULL) = (fork_boundary_seq IS NULL)) NOT VALID;
  END IF;
END $$;
