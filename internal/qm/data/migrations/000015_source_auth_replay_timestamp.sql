DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'source_auth_replay'
      AND column_name = 'expires_at'
      AND data_type = 'bigint'
  ) THEN
    ALTER TABLE source_auth_replay
      ALTER COLUMN expires_at TYPE TIMESTAMPTZ
      USING to_timestamp(expires_at / 1000.0);
  END IF;
END $$;
