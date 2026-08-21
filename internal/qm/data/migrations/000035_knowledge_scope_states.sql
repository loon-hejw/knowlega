ALTER TABLE knowledge_scopes ALTER COLUMN status SET DEFAULT 'empty';
UPDATE knowledge_scopes SET status = 'empty' WHERE status = 'active';
