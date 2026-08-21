package postgres

import (
	"strings"
	"testing"
)

func TestBootstrapSQLIncludesLLMWikiDerivedStateTables(t *testing.T) {
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS wiki_page_versions",
		"CREATE TABLE IF NOT EXISTS source_manifest",
		"CREATE TABLE IF NOT EXISTS review_items",
		"CREATE TABLE IF NOT EXISTS query_logs",
		"model_id text",
		"duration_ms bigint",
		"CREATE TABLE IF NOT EXISTS code_repos",
		"CREATE TABLE IF NOT EXISTS graph_nodes",
		"CREATE TABLE IF NOT EXISTS graph_edges",
		"CREATE EXTENSION IF NOT EXISTS vector",
		"embedding_model",
		"embedding_source_sha256",
		"embedding_updated_at",
		"frontmatter->>'aliases'",
		"wiki_pages_search_idx",
		"source_manifest_sha_idx",
		"source_manifest_archive_idx",
		"archive_path",
		"original_raw_path",
		"content_path",
		"pipeline_version",
		"generation_contract_sha256",
		"new_page_budget",
		"new_page_count",
		"created_pages",
		"qm_file_id",
		"qm_project_id",
		"qm_scope_id",
		"qm_source_sha256",
		"wiki_page_versions_page_idx",
		"graph_edges_src_idx",
		"graph_nodes_domain_scope_idx",
		"graph_edges_domain_scope_idx",
		"confidence_score",
		"evidence text[]",
		"ALTER TABLE scope_bindings ALTER COLUMN status SET DEFAULT 'empty'",
	} {
		if !strings.Contains(BootstrapSQL, want) {
			t.Fatalf("BootstrapSQL missing %q", want)
		}
	}
}

func TestWikiEvidenceSearchSQLBoostsAliases(t *testing.T) {
	for _, want := range []string{
		"frontmatter->>'aliases'",
		"aliases_text LIKE",
		"THEN 320",
		"sources_text LIKE",
		"unnest(q.terms)",
		"pages.haystack LIKE",
	} {
		if !strings.Contains(wikiEvidenceSearchSQL, want) {
			t.Fatalf("wikiEvidenceSearchSQL missing %q:\n%s", want, wikiEvidenceSearchSQL)
		}
	}
}

func TestVectorLiteral(t *testing.T) {
	got := vectorLiteral([]float32{0.125, -1.5, 2})
	if got != "[0.125,-1.5,2]" {
		t.Fatalf("vector literal=%s", got)
	}
}
