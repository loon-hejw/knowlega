package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type Store struct {
	db   dbExecutor
	root *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, root: db}
}

type dbExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) WithWikiPageStoreTx(ctx context.Context, fn func(core.WikiPageStore) error) error {
	if s.root == nil {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txStore := &Store{db: tx, root: s.root}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) WithCodeGraphStoreTx(ctx context.Context, fn func(core.CodeGraphStore) error) error {
	if s.root == nil {
		return fn(s)
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txStore := &Store{db: tx, root: s.root}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, BootstrapSQL)
	return err
}

func (s *Store) UpsertProject(ctx context.Context, p core.Project) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO projects (id, name, root_path, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
  name = EXCLUDED.name,
  root_path = EXCLUDED.root_path,
  updated_at = EXCLUDED.updated_at
`, p.ID, p.Name, p.RootPath, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) UpsertScopeBinding(ctx context.Context, binding core.ScopeBinding) error {
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = time.Now()
	}
	if binding.UpdatedAt.IsZero() {
		binding.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO scope_bindings (provider, external_scope_id, kind, organization_id, project_id, project_name, root_path, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (provider, external_scope_id) DO UPDATE SET
  kind = EXCLUDED.kind,
  organization_id = EXCLUDED.organization_id,
  project_id = EXCLUDED.project_id,
  project_name = EXCLUDED.project_name,
  root_path = EXCLUDED.root_path,
  status = EXCLUDED.status,
  updated_at = EXCLUDED.updated_at
`, binding.Provider, binding.ExternalScopeID, binding.Kind, binding.OrganizationID, binding.ProjectID,
		binding.ProjectName, binding.RootPath, binding.Status, binding.CreatedAt, binding.UpdatedAt)
	return err
}

func (s *Store) GetScopeBinding(ctx context.Context, provider, externalScopeID string) (core.ScopeBinding, error) {
	var binding core.ScopeBinding
	err := s.db.QueryRowContext(ctx, `
SELECT provider, external_scope_id, kind, organization_id, project_id, project_name, root_path, status, created_at, updated_at
FROM scope_bindings
WHERE provider = $1 AND external_scope_id = $2
`, provider, externalScopeID).Scan(
		&binding.Provider, &binding.ExternalScopeID, &binding.Kind, &binding.OrganizationID,
		&binding.ProjectID, &binding.ProjectName, &binding.RootPath, &binding.Status,
		&binding.CreatedAt, &binding.UpdatedAt,
	)
	return binding, err
}

func (s *Store) CountProjectDocuments(ctx context.Context, projectID string) (pages, sources int64, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM wiki_pages WHERE project_id = $1`, projectID).Scan(&pages); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sources WHERE project_id = $1`, projectID).Scan(&sources); err != nil {
		return 0, 0, err
	}
	return pages, sources, nil
}

func (s *Store) UpsertSource(ctx context.Context, src core.Source) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sources (id, project_id, path, kind, title, sha256, immutable, original_path, imported_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (project_id, path) DO UPDATE SET
  kind = EXCLUDED.kind,
  title = EXCLUDED.title,
  sha256 = EXCLUDED.sha256,
  immutable = EXCLUDED.immutable,
  original_path = EXCLUDED.original_path,
  imported_at = EXCLUDED.imported_at
`, src.ID, src.ProjectID, src.Path, src.Kind, src.Title, src.SHA256, src.Immutable, src.OriginalPath, src.ImportedAt)
	return err
}

func (s *Store) DeleteSourcesNotIn(ctx context.Context, projectID string, ids []string) error {
	if len(ids) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE project_id = $1`, projectID)
		return err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, projectID)
	placeholders := make([]string, 0, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM sources
WHERE project_id = $1
  AND id NOT IN (%s)
`, strings.Join(placeholders, ", ")), args...)
	return err
}

func (s *Store) UpsertWikiPage(ctx context.Context, page core.WikiPage) error {
	fm, err := json.Marshal(page.Frontmatter)
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO wiki_pages (id, project_id, path, type, title, body, frontmatter, sources, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
ON CONFLICT (project_id, path) DO UPDATE SET
  type = EXCLUDED.type,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  frontmatter = EXCLUDED.frontmatter,
  sources = EXCLUDED.sources,
  updated_at = EXCLUDED.updated_at
`, page.ID, page.ProjectID, page.Path, page.Type, page.Title, page.Body, string(fm), sqlArray(page.Sources), page.UpdatedAt)
	return err
}

func (s *Store) UpsertWikiPages(ctx context.Context, pages []core.WikiPage) error {
	for _, page := range pages {
		if err := s.UpsertWikiPage(ctx, page); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteWikiPagesNotIn(ctx context.Context, projectID string, paths []string) error {
	if len(paths) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM wiki_pages WHERE project_id = $1`, projectID)
		return err
	}
	args := make([]any, 0, len(paths)+1)
	args = append(args, projectID)
	placeholders := make([]string, 0, len(paths))
	for i, path := range paths {
		args = append(args, path)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM wiki_pages
WHERE project_id = $1
  AND path NOT IN (%s)
`, strings.Join(placeholders, ", ")), args...)
	return err
}

func (s *Store) WikiPageEmbeddingStatus(ctx context.Context, projectID, path string) (core.WikiPageEmbeddingStatus, error) {
	var status core.WikiPageEmbeddingStatus
	err := s.db.QueryRowContext(ctx, `
SELECT embedding_model, embedding_source_sha256
FROM wiki_pages
WHERE project_id = $1 AND path = $2
`, projectID, path).Scan(&status.Model, &status.SourceSHA256)
	if err == sql.ErrNoRows {
		return core.WikiPageEmbeddingStatus{}, nil
	}
	if err != nil {
		return core.WikiPageEmbeddingStatus{}, err
	}
	return status, nil
}

func (s *Store) UpsertWikiPageEmbedding(ctx context.Context, projectID, path string, embedding []float32, meta core.WikiPageEmbeddingMetadata) error {
	if len(embedding) == 0 {
		return fmt.Errorf("embedding is empty")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE wiki_pages
SET embedding = $3::vector,
    embedding_model = $4,
    embedding_source_sha256 = $5,
    embedding_updated_at = now()
WHERE project_id = $1 AND path = $2
`, projectID, path, vectorLiteral(embedding), meta.Model, meta.SourceSHA256)
	return err
}

func (s *Store) InsertWikiPageVersion(ctx context.Context, version core.WikiPageVersion) error {
	fm, err := json.Marshal(version.Frontmatter)
	if err != nil {
		return fmt.Errorf("marshal version frontmatter: %w", err)
	}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now()
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO wiki_page_versions (id, project_id, page_id, path, body, frontmatter, sources, reason, created_at)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)
ON CONFLICT (id) DO NOTHING
`, version.ID, version.ProjectID, version.PageID, version.Path, version.Body, string(fm), sqlArray(version.Sources), version.Reason, version.CreatedAt)
	return err
}

func (s *Store) DeleteWikiPageVersionsNotIn(ctx context.Context, projectID string, ids []string) error {
	if len(ids) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM wiki_page_versions WHERE project_id = $1`, projectID)
		return err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, projectID)
	placeholders := make([]string, 0, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM wiki_page_versions
WHERE project_id = $1
  AND id NOT IN (%s)
`, strings.Join(placeholders, ", ")), args...)
	return err
}

func (s *Store) UpsertSourceManifestEntry(ctx context.Context, entry core.SourceManifestEntry) error {
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO source_manifest (id, project_id, original_path, pipeline_version, sha256, raw_path, archive_path, original_raw_path, content_path, original_sha256, content_sha256, title, files, generation_contract_sha256, new_page_budget, new_page_count, created_pages, review_count, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (project_id, original_path) DO UPDATE SET
  pipeline_version = EXCLUDED.pipeline_version,
  sha256 = EXCLUDED.sha256,
  raw_path = EXCLUDED.raw_path,
  archive_path = EXCLUDED.archive_path,
  original_raw_path = EXCLUDED.original_raw_path,
  content_path = EXCLUDED.content_path,
  original_sha256 = EXCLUDED.original_sha256,
  content_sha256 = EXCLUDED.content_sha256,
  title = EXCLUDED.title,
  files = EXCLUDED.files,
  generation_contract_sha256 = EXCLUDED.generation_contract_sha256,
  new_page_budget = EXCLUDED.new_page_budget,
  new_page_count = EXCLUDED.new_page_count,
  created_pages = EXCLUDED.created_pages,
  review_count = EXCLUDED.review_count,
  updated_at = EXCLUDED.updated_at
`, entry.ID, entry.ProjectID, entry.OriginalPath, entry.PipelineVersion, entry.SHA256, entry.RawPath, entry.ArchivePath, entry.OriginalRawPath, entry.ContentPath, entry.OriginalSHA256, entry.ContentSHA256, entry.Title, sqlArray(entry.Files), entry.GenerationContractSHA256, entry.NewPageBudget, entry.NewPageCount, sqlArray(entry.CreatedPages), entry.ReviewCount, entry.UpdatedAt)
	return err
}

func (s *Store) DeleteSourceManifestEntriesNotIn(ctx context.Context, projectID string, ids []string) error {
	if len(ids) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM source_manifest WHERE project_id = $1`, projectID)
		return err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, projectID)
	placeholders := make([]string, 0, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM source_manifest
WHERE project_id = $1
  AND id NOT IN (%s)
`, strings.Join(placeholders, ", ")), args...)
	return err
}

func (s *Store) UpsertCodeRepo(ctx context.Context, repo core.CodeRepo) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO code_repos (id, project_id, repo_path, head_commit, indexed_commit, primary_graph_source, stale, synced_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET
  repo_path = EXCLUDED.repo_path,
  head_commit = EXCLUDED.head_commit,
  indexed_commit = EXCLUDED.indexed_commit,
  primary_graph_source = EXCLUDED.primary_graph_source,
  stale = EXCLUDED.stale,
  synced_at = EXCLUDED.synced_at
`, repo.ID, repo.ProjectID, repo.RepoPath, repo.HeadCommit, repo.IndexedCommit, repo.PrimaryGraphSource, repo.Stale, repo.SyncedAt)
	return err
}

func (s *Store) DeleteGraphFacts(ctx context.Context, projectID, repoID string) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM graph_edges
WHERE project_id = $1 AND repo_id = $2
`, projectID, nullIfEmpty(repoID)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM graph_nodes
WHERE project_id = $1 AND repo_id = $2
`, projectID, nullIfEmpty(repoID))
	return err
}

func (s *Store) UpsertReviewItem(ctx context.Context, item core.ReviewItem) error {
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now()
	}
	props, err := json.Marshal(map[string]any{
		"source_path": item.SourcePath, "source_paths": item.SourcePaths,
		"search_queries": item.SearchQueries, "resolved_action": item.ResolvedAction,
	})
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO review_items (id, project_id, type, title, description, severity, status, affected_pages, props, created_at, resolved_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
  type = EXCLUDED.type,
  title = EXCLUDED.title,
  description = EXCLUDED.description,
  severity = EXCLUDED.severity,
  status = EXCLUDED.status,
  affected_pages = EXCLUDED.affected_pages,
  props = EXCLUDED.props,
  resolved_at = EXCLUDED.resolved_at
`, item.ID, item.ProjectID, item.Type, item.Title, item.Description, item.Severity, item.Status, sqlArray(item.AffectedPages), props, item.CreatedAt, item.ResolvedAt)
	return err
}

func (s *Store) DeleteReviewItemsNotIn(ctx context.Context, projectID string, ids []string) error {
	if len(ids) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM review_items WHERE project_id = $1`, projectID)
		return err
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, projectID)
	placeholders := make([]string, 0, len(ids))
	for i, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM review_items
WHERE project_id = $1
  AND id NOT IN (%s)
`, strings.Join(placeholders, ", ")), args...)
	return err
}

func (s *Store) InsertQueryLog(ctx context.Context, id, projectID, query, mode string, resultCount int) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO query_logs (id, project_id, query, mode, result_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
`, id, projectID, query, mode, resultCount)
	return err
}

func (s *Store) AddGraphNode(ctx context.Context, node core.GraphNode) error {
	props, err := json.Marshal(node.Props)
	if err != nil {
		return fmt.Errorf("marshal graph node props: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO graph_nodes (id, project_id, repo_id, domain, scope_id, kind, label, source_ref, props)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
ON CONFLICT (id) DO UPDATE SET
  repo_id = EXCLUDED.repo_id,
  domain = EXCLUDED.domain,
  scope_id = EXCLUDED.scope_id,
  kind = EXCLUDED.kind,
  label = EXCLUDED.label,
  source_ref = EXCLUDED.source_ref,
  props = EXCLUDED.props
`, node.ID, node.ProjectID, nullIfEmpty(node.RepoID), defaultString(node.Domain, "code"), node.ScopeID, node.Kind, node.Label, node.SourceRef, string(props))
	return err
}

func (s *Store) AddGraphEdge(ctx context.Context, edge core.GraphEdge) error {
	props, err := json.Marshal(edge.Props)
	if err != nil {
		return fmt.Errorf("marshal graph edge props: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO graph_edges (id, project_id, repo_id, domain, scope_id, src_id, dst_id, relation, confidence, confidence_score, weight, evidence, props)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)
ON CONFLICT (id) DO UPDATE SET
  repo_id = EXCLUDED.repo_id,
  domain = EXCLUDED.domain,
  scope_id = EXCLUDED.scope_id,
  relation = EXCLUDED.relation,
  confidence = EXCLUDED.confidence,
  confidence_score = EXCLUDED.confidence_score,
  weight = EXCLUDED.weight,
  evidence = EXCLUDED.evidence,
  props = EXCLUDED.props
`, edge.ID, edge.ProjectID, nullIfEmpty(edge.RepoID), defaultString(edge.Domain, "code"), edge.ScopeID, edge.SourceID, edge.TargetID, edge.Relation, edge.Confidence, edge.ConfidenceScore, edge.Weight, sqlArray(edge.Evidence), string(props))
	return err
}

func (s *Store) SearchGraphEvidence(ctx context.Context, projectID, q string, limit int) ([]core.GraphEvidence, error) {
	terms := graphEvidenceTerms(q)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	args := []any{projectID}
	clauses := make([]string, 0, len(terms))
	for _, term := range terms {
		args = append(args, "%"+strings.ToLower(term)+"%")
		clauses = append(clauses, fmt.Sprintf("haystack LIKE $%d", len(args)))
	}
	args = append(args, limit)
	query := fmt.Sprintf(`
WITH graph_evidence AS (
  SELECT
    COALESCE(NULLIF(gn.source_ref, ''), 'pg/graph_nodes/' || gn.id) AS path,
    gn.kind || ' ' || gn.label AS title,
    lower(concat_ws(' ', gn.id, gn.kind, gn.label, gn.source_ref, gn.props::text, cr.repo_path, cr.indexed_commit)) AS haystack,
    concat(
      'Graph node evidence', chr(10), chr(10),
      'Repo ID: ', COALESCE(gn.repo_id, ''), chr(10),
      'Repo path: ', COALESCE(cr.repo_path, ''), chr(10),
      'Indexed commit: ', COALESCE(cr.indexed_commit, ''), chr(10),
      'Node ID: ', gn.id, chr(10),
      'Kind: ', gn.kind, chr(10),
      'Label: ', gn.label, chr(10),
      'Source ref: ', gn.source_ref, chr(10),
      'Props: ', gn.props::text
    ) AS content,
    80 AS score
  FROM graph_nodes gn
  LEFT JOIN code_repos cr ON cr.id = gn.repo_id
  WHERE gn.project_id = $1
  UNION ALL
  SELECT
    COALESCE(NULLIF(ge.props->>'source_ref', ''), 'pg/graph_edges/' || ge.id) AS path,
    COALESCE(src.label, ge.src_id) || ' -> ' || COALESCE(dst.label, ge.dst_id) || ' ' || ge.relation AS title,
    lower(concat_ws(' ', ge.id, ge.domain, ge.scope_id, ge.src_id, ge.dst_id, ge.relation, ge.confidence, ge.confidence_score::text, array_to_string(ge.evidence, ' '), ge.props::text, src.label, dst.label, src.source_ref, dst.source_ref, cr.repo_path, cr.indexed_commit)) AS haystack,
    concat(
      'Graph edge evidence', chr(10), chr(10),
      'Repo ID: ', COALESCE(ge.repo_id, ''), chr(10),
      'Repo path: ', COALESCE(cr.repo_path, ''), chr(10),
      'Indexed commit: ', COALESCE(cr.indexed_commit, ''), chr(10),
      'Source: ', ge.src_id, ' (', COALESCE(src.label, ''), ')', chr(10),
      'Target: ', ge.dst_id, ' (', COALESCE(dst.label, ''), ')', chr(10),
      'Relation: ', ge.relation, chr(10),
      'Confidence: ', ge.confidence, chr(10),
      'Confidence score: ', ge.confidence_score, chr(10),
      'Weight: ', ge.weight, chr(10),
      'Evidence: ', array_to_string(ge.evidence, ', '), chr(10),
      'Props: ', ge.props::text
    ) AS content,
    120 AS score
  FROM graph_edges ge
  LEFT JOIN graph_nodes src ON src.id = ge.src_id
  LEFT JOIN graph_nodes dst ON dst.id = ge.dst_id
  LEFT JOIN code_repos cr ON cr.id = ge.repo_id
  WHERE ge.project_id = $1
)
SELECT path, title, content, score
FROM graph_evidence
WHERE %s
ORDER BY score DESC, title
LIMIT $%d
`, strings.Join(clauses, " OR "), len(args))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evidence []core.GraphEvidence
	for rows.Next() {
		var item core.GraphEvidence
		if err := rows.Scan(&item.Path, &item.Title, &item.Content, &item.Score); err != nil {
			return nil, err
		}
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return evidence, nil
}

func (s *Store) SearchWikiEvidence(ctx context.Context, projectID string, plan core.QueryPlan, limit int) ([]core.QueryResult, error) {
	searchText := searchTextFromPlan(plan)
	if searchText == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = plan.CandidateLimit
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, wikiEvidenceSearchSQL, projectID, searchText, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []core.QueryResult
	for rows.Next() {
		var result core.QueryResult
		if err := rows.Scan(&result.Path, &result.Title, &result.Kind, &result.Snippet, &result.Score); err != nil {
			return nil, err
		}
		if result.Kind == "" {
			result.Kind = "wiki"
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

const wikiEvidenceSearchSQL = `
WITH q AS (
  SELECT plainto_tsquery('simple', $2) AS tsq, lower($2) AS raw
),
pages AS (
  SELECT
    wp.*,
    lower(coalesce(wp.frontmatter->>'aliases', '')) AS aliases_text,
    lower(concat_ws(' ', wp.title, wp.body, wp.frontmatter::text, array_to_string(wp.sources, ' '))) AS haystack
  FROM wiki_pages wp
  WHERE wp.project_id = $1
),
ranked AS (
  SELECT
    pages.path,
    pages.title,
    pages.type,
    left(pages.body, 320) AS snippet,
    (
      (ts_rank(pages.search_vector, q.tsq) * 1000)::int +
      CASE WHEN lower(pages.title) LIKE '%' || q.raw || '%' THEN 160 ELSE 0 END +
      CASE WHEN pages.aliases_text LIKE '%' || q.raw || '%' THEN 320 ELSE 0 END +
      CASE WHEN lower(pages.frontmatter::text) LIKE '%' || q.raw || '%' THEN 120 ELSE 0 END +
      CASE WHEN lower(pages.body) LIKE '%' || q.raw || '%' THEN 80 ELSE 0 END
    ) AS score
  FROM pages, q
  WHERE (
      pages.search_vector @@ q.tsq
      OR pages.haystack LIKE '%' || q.raw || '%'
  )
)
SELECT path, title, type, snippet, score
FROM ranked
ORDER BY score DESC, path
LIMIT $3
`

func (s *Store) SearchWikiEvidenceVector(ctx context.Context, projectID string, plan core.QueryPlan, embedding []float32, limit int) ([]core.QueryResult, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = plan.CandidateLimit
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
  path,
  title,
  type,
  left(body, 320) AS snippet,
  GREATEST(1, (1000 - ((embedding <=> $2::vector) * 1000))::int) AS score
FROM wiki_pages
WHERE project_id = $1 AND embedding IS NOT NULL
ORDER BY embedding <=> $2::vector, path
LIMIT $3
`, projectID, vectorLiteral(embedding), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []core.QueryResult
	for rows.Next() {
		var result core.QueryResult
		if err := rows.Scan(&result.Path, &result.Title, &result.Kind, &result.Snippet, &result.Score); err != nil {
			return nil, err
		}
		if result.Kind == "" {
			result.Kind = "wiki"
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func searchTextFromPlan(plan core.QueryPlan) string {
	var parts []string
	for _, search := range plan.Searches {
		if strings.TrimSpace(search.Text) != "" {
			parts = append(parts, strings.TrimSpace(search.Text))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return strings.TrimSpace(plan.Question)
}

func vectorLiteral(values []float32) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func graphEvidenceTerms(q string) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	seen := map[string]bool{}
	var terms []string
	add := func(term string) {
		term = strings.Trim(strings.TrimSpace(term), `"'.,:;()[]{}<>`)
		if term == "" || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	add(q)
	for _, field := range strings.Fields(q) {
		add(field)
	}
	return terms
}

// sqlArray keeps the core driver-agnostic. The default lib/pq and pgx stdlib
// drivers both accept []string for text[] parameters.
func sqlArray(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
