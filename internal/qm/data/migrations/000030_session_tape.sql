CREATE TABLE IF NOT EXISTS session_tape (
  session_id TEXT NOT NULL,
  seq INT NOT NULL,
  kind TEXT NOT NULL,
  harness TEXT,
  payload TEXT NOT NULL,
  scope_label TEXT NOT NULL,
  bare_text TEXT,
  ts TEXT,
  change_time TEXT,
  hidden BOOLEAN,
  overheard BOOLEAN,
  author TEXT,
  entry_seq INT,
  covers_entry_seq INT,
  created_at BIGINT NOT NULL,
  PRIMARY KEY(session_id, seq)
);

CREATE INDEX IF NOT EXISTS session_tape_by_session_created
  ON session_tape(session_id, created_at, seq);
