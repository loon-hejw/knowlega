package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	knowledgecore "github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	knowledgeservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type acceptanceSessionStore struct {
	entries []data.SessionEntry
}

func (s *acceptanceSessionStore) Entries(_ context.Context, _ string, _ int, since int) ([]data.SessionEntry, error) {
	result := make([]data.SessionEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.Sequence >= since {
			result = append(result, entry)
		}
	}
	return result, nil
}

func (s *acceptanceSessionStore) record(call ToolCall, result ToolResult) {
	s.entries = append(s.entries, data.SessionEntry{
		Sequence: len(s.entries),
		Type:     "tool_result",
		Payload:  toolResultEntryPayload(call, result),
	})
}

func TestXiyoujiKnowledgeToolWritesNineRequirementVerifyReport(t *testing.T) {
	root := t.TempDir()
	knowledgeAgent, err := knowlega.New(knowlega.AgentOptions{RootDir: root, Compiler: compiler.MockProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	ref := knowlega.ScopeRef{OrgID: "acme", ExternalScopeID: "group:web-project-xiyouji", Kind: "project", Name: "西游记验收"}
	status, err := knowledgeAgent.EnsureScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join("..", "..", "..", "tst", "xiyouji-chapters")
	compiled, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath: status.ProjectPath,
		SourcePath:  sourceDir,
		Provider:    compiler.MockProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err = knowledgeAgent.RefreshScope(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.SourceCount != 100 || compiled.FileCount != 300 || status.SourceCount != 100 || status.State != knowledgeservice.KnowledgeWorkspaceReady {
		t.Fatalf("compiled=%+v status=%+v", compiled, status)
	}
	if summaries := countAcceptanceMarkdown(t, filepath.Join(status.ProjectPath, "wiki", "sources")); summaries != 100 {
		t.Fatalf("source summaries=%d, want 100", summaries)
	}
	for _, rel := range []string{"wiki/index.md", "wiki/overview.md", "wiki/log.md", "wiki/reviews.md"} {
		info, statErr := os.Stat(filepath.Join(status.ProjectPath, filepath.FromSlash(rel)))
		if statErr != nil || info.Size() == 0 {
			t.Fatalf("required artifact %s info=%v err=%v", rel, info, statErr)
		}
	}
	overviewPath := filepath.Join(status.ProjectPath, "wiki", "overview.md")
	overview, err := os.ReadFile(overviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := wiki.WriteVersionedPage(status.ProjectPath, "wiki/overview.md", append(overview, []byte("\nAcceptance checkpoint.\n")...), "xiyouji acceptance version check"); err != nil {
		t.Fatal(err)
	}
	pageVersions := countAcceptanceFiles(t, filepath.Join(status.ProjectPath, ".kbcore", "page-versions"))
	if pageVersions == 0 {
		t.Fatal("expected version archives for updated navigation artifacts")
	}

	corpusResults, err := knowledgeAgent.Search(t.Context(), ref, "灵根育孕源流出", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"raw/", "wiki/sources/", "wiki/concepts/", "wiki/entities/"} {
		path := acceptanceResultPath(corpusResults, prefix)
		if path == "" {
			t.Fatalf("search results missing %s evidence: %+v", prefix, corpusResults)
		}
		if _, err := knowledgeAgent.Read(ref, path); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
	}

	issues, err := knowledgeservice.LintWiki(status.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	brokenLinks := 0
	for _, issue := range issues {
		if issue.Type == "broken-link" {
			brokenLinks++
		}
	}
	if brokenLinks != 0 {
		t.Fatalf("broken wikilinks=%d issues=%+v", brokenLinks, issues)
	}
	reviews, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID})
	if err != nil || len(reviews) != 100 {
		t.Fatalf("reviews=%d err=%v", len(reviews), err)
	}

	sessions := &acceptanceSessionStore{entries: []data.SessionEntry{{Sequence: 0, Type: "user", Payload: json.RawMessage(`{"text":"九条件验收"}`)}}}
	resolver, err := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessions, Knowledge: knowledgeAgent})
	if err != nil {
		t.Fatal(err)
	}
	turnStart := 0
	tools, err := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{
		SessionID: "xiyouji-acceptance", ScopeLabel: ref.ExternalScopeID, OrgScopeID: "org:" + ref.OrgID, UserEntrySequence: &turnStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := "wiki/entities/chapter-001-entity.md"
	candidateDoc, err := knowledgeAgent.Read(ref, candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	requirements := []knowledgecore.KnowledgeRequirement{
		{ID: "r1", Text: "灵根育孕", Kind: "positive"},
		{ID: "r2", Text: "心性修持", Kind: "positive"},
		{ID: "r3", Text: "花果山", Kind: "positive"},
		{ID: "r4", Text: "石猴", Kind: "positive"},
		{ID: "r5", Text: "水帘洞", Kind: "positive"},
		{ID: "r6", Text: "傲来国", Kind: "positive"},
		{ID: "r7", Text: "仙石", Kind: "positive"},
		{ID: "r8", Text: "盘古", Kind: "positive"},
		{ID: "r9", Text: "zzqvnegativeevidence9f3c2a", Kind: "negative"},
	}
	searches := 0
	for _, requirement := range requirements {
		call := acceptanceKnowledgeCall(t, "search", map[string]any{
			"action": "search", "query": requirement.Text, "candidate": candidateDoc.Title, "requirement_id": requirement.ID, "limit": 20,
		})
		result, err := tools.Execute(t.Context(), call)
		if err != nil || result.IsError {
			t.Fatalf("requirement %s search result=%+v err=%v", requirement.ID, result, err)
		}
		var matches []knowledgecore.KnowledgeSearchResult
		if err := json.Unmarshal([]byte(toolResultText(result)), &matches); err != nil {
			t.Fatalf("requirement %s search JSON=%s err=%v", requirement.ID, toolResultText(result), err)
		}
		if requirement.Kind == "positive" && len(matches) == 0 {
			t.Fatalf("positive requirement %s returned no candidates", requirement.ID)
		}
		if requirement.Kind == "negative" && len(matches) != 0 {
			t.Fatalf("negative requirement %s unexpectedly matched %+v", requirement.ID, matches)
		}
		sessions.record(call, result)
		searches++
	}
	readCall := acceptanceKnowledgeCall(t, "read", map[string]any{"action": "read", "path": candidatePath})
	readResult, err := tools.Execute(t.Context(), readCall)
	if err != nil || readResult.IsError {
		t.Fatalf("candidate read=%+v err=%v", readResult, err)
	}
	sessions.record(readCall, readResult)

	checks := make([]knowledgecore.KnowledgeEvidenceCheck, 0, len(requirements))
	for _, requirement := range requirements {
		check := knowledgecore.KnowledgeEvidenceCheck{RequirementID: requirement.ID, Status: "supported", EvidencePaths: []string{candidatePath}}
		if requirement.Kind == "negative" {
			check.Status = "not_found_in_corpus"
			check.EvidencePaths = nil
		}
		checks = append(checks, check)
	}
	submitCall := acceptanceKnowledgeCall(t, "submit", map[string]any{
		"action": "submit", "question": "哪个候选满足九个验收条件？", "answer": candidateDoc.Title, "candidate": candidateDoc.Title,
		"requirements": requirements, "checks": checks, "evidence_paths": []string{candidatePath},
	})
	submitResult, err := tools.Execute(t.Context(), submitCall)
	if err != nil || submitResult.IsError {
		t.Fatalf("submit=%+v err=%v", submitResult, err)
	}
	var submission knowledgecore.KnowledgeSubmission
	if err := json.Unmarshal([]byte(toolResultText(submitResult)), &submission); err != nil {
		t.Fatal(err)
	}
	if submission.Status != "complete" || len(submission.UnresolvedRequirementIDs) != 0 || len(submission.Citations) == 0 || searches != 9 {
		t.Fatalf("submission=%+v searches=%d", submission, searches)
	}

	linkCount := countAcceptanceWikilinks(t, filepath.Join(status.ProjectPath, "wiki"))
	report := fmt.Sprintf(`# VERIFY_REPORT

- Sources: %d
- Generated pages: %d
- Wiki pages: %d
- Source summaries: %d
- Page versions: %d
- Wikilinks: %d
- Lint issues: %d
- Broken wikilinks: %d
- Review items: %d
- Requirements: %d
- Candidate-specific searches: %d
- Evidence-backed positive checks: %d
- Negative zero-hit checks: %d
- Submit status: %s
`, compiled.SourceCount, compiled.FileCount, status.WikiPageCount, 100, pageVersions, linkCount, len(issues), brokenLinks, len(reviews), len(requirements), searches, 8, 1, submission.Status)
	reportPath := filepath.Join(status.ProjectPath, "VERIFY_REPORT.md")
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(reportPath)
	if err != nil || !strings.Contains(string(written), "- Sources: 100") || !strings.Contains(string(written), "- Generated pages: 300") || !strings.Contains(string(written), "- Requirements: 9") || !strings.Contains(string(written), "- Submit status: complete") {
		t.Fatalf("verify report=%s err=%v", written, err)
	}
}

func acceptanceKnowledgeCall(t *testing.T, id string, value map[string]any) ToolCall {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{ID: id, Name: "knowledge", Arguments: raw}
}

func acceptanceResultPath(results []knowledgecore.KnowledgeSearchResult, prefix string) string {
	for _, result := range results {
		if strings.HasPrefix(result.Path, prefix) {
			return result.Path
		}
	}
	return ""
}

func countAcceptanceMarkdown(t *testing.T, root string) int {
	t.Helper()
	return countAcceptanceMatching(t, root, func(path string) bool { return strings.EqualFold(filepath.Ext(path), ".md") })
}

func countAcceptanceFiles(t *testing.T, root string) int {
	t.Helper()
	return countAcceptanceMatching(t, root, func(string) bool { return true })
}

func countAcceptanceMatching(t *testing.T, root string, match func(string) bool) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && match(path) {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func countAcceptanceWikilinks(t *testing.T, root string) int {
	t.Helper()
	links := 0
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		links += strings.Count(string(content), "[[")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return links
}
