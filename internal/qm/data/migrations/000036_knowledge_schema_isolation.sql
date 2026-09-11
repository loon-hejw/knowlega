-- Resolve the `projects` table-name collision between the QM control plane and
-- the Knowlega agent.
--
-- Both bootstraps declared `CREATE TABLE IF NOT EXISTS projects` against the
-- same database with different shapes:
--
--   QM        projects(id TEXT PRIMARY KEY, json JSONB NOT NULL)
--   Knowlega  projects(id, name, root_path, created_at, updated_at)
--
-- Whichever ran first won and the other was silently skipped, so
-- `000001_control.sql` was recorded as applied while `public.projects` actually
-- carried the Knowlega shape. Every QM `SELECT json FROM projects` then failed
-- with `column "json" does not exist` (SQLSTATE 42703).
--
-- Knowlega now owns the dedicated `knowledge_core` schema, so the two tables no
-- longer share a namespace. This migration relocates pre-isolation Knowlega
-- tables out of `public` into `knowledge_core` and restores QM's own
-- `public.projects`.
--
-- Relocation is deliberately conservative: it runs only when `public.projects`
-- really is the Knowlega table and `knowledge_core` still holds no rows. It
-- never drops a table that has data in it, and re-running is a no-op.

CREATE SCHEMA IF NOT EXISTS knowledge_core;

DO $$
DECLARE
  knowlega_tables TEXT[] := ARRAY[
    'projects', 'scope_bindings', 'sources', 'wiki_pages', 'wiki_page_versions',
    'source_manifest', 'code_repos', 'graph_nodes', 'graph_edges',
    'ingest_jobs', 'review_items', 'query_logs'
  ];
  tbl TEXT;
  row_count BIGINT;
  legacy_rows BIGINT := 0;
  target_rows BIGINT := 0;
  legacy_is_knowlega BOOLEAN;
BEGIN
  -- `public.projects` is the Knowlega table only if it has Knowlega's
  -- `root_path` column. If QM's shape is already there, nothing to relocate.
  SELECT EXISTS (
    SELECT 1 FROM information_schema.columns c
    WHERE c.table_schema = 'public'
      AND c.table_name = 'projects'
      AND c.column_name = 'root_path'
  ) INTO legacy_is_knowlega;

  IF NOT legacy_is_knowlega THEN
    RAISE NOTICE 'knowledge isolation: public.projects is not the Knowlega table; nothing to relocate';
    RETURN;
  END IF;

  -- Count rows on both sides before touching anything.
  FOREACH tbl IN ARRAY knowlega_tables LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables t
               WHERE t.table_schema = 'public' AND t.table_name = tbl) THEN
      EXECUTE format('SELECT count(*) FROM public.%I', tbl) INTO row_count;
      legacy_rows := legacy_rows + row_count;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables t
               WHERE t.table_schema = 'knowledge_core' AND t.table_name = tbl) THEN
      EXECUTE format('SELECT count(*) FROM knowledge_core.%I', tbl) INTO row_count;
      target_rows := target_rows + row_count;
    END IF;
  END LOOP;

  IF target_rows > 0 THEN
    -- knowledge_core already carries data. Never clobber it; leave the legacy
    -- public tables in place for manual reconciliation instead.
    RAISE NOTICE 'knowledge isolation: knowledge_core already holds % row(s); leaving public tables untouched', target_rows;
    RETURN;
  END IF;

  -- knowledge_core holds only freshly bootstrapped empty tables. Drop those
  -- placeholders, then move the populated public tables across. All twelve move
  -- together so their foreign keys stay intact.
  FOREACH tbl IN ARRAY knowlega_tables LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables t
               WHERE t.table_schema = 'knowledge_core' AND t.table_name = tbl) THEN
      EXECUTE format('DROP TABLE knowledge_core.%I CASCADE', tbl);
    END IF;
  END LOOP;

  FOREACH tbl IN ARRAY knowlega_tables LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables t
               WHERE t.table_schema = 'public' AND t.table_name = tbl) THEN
      EXECUTE format('ALTER TABLE public.%I SET SCHEMA knowledge_core', tbl);
    END IF;
  END LOOP;

  RAISE NOTICE 'knowledge isolation: relocated % Knowlega row(s) from public to knowledge_core', legacy_rows;
END
$$;

-- `000001_control.sql` was recorded as applied while its
-- `CREATE TABLE IF NOT EXISTS projects` was skipped by the Knowlega table.
-- Now that `public.projects` is free, create QM's control-plane shape.
CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, json JSONB NOT NULL);