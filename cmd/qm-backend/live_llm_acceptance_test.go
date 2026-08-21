package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	knowledgecore "github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
	qmagent "github.com/loon-hejw/knowlega/internal/qm/agent"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	qmserver "github.com/loon-hejw/knowlega/internal/qm/server"
)

// TestRealLLMQMKnowledgePiAcceptance is deliberately credential-gated. It is
// the authoritative acceptance for the production path that mocks cannot
// prove: QM HTTP upload -> project-file worker -> real LLM Wiki compilation ->
// PostgreSQL projection -> real Pi knowledge search/read/submit.
func TestRealLLMQMKnowledgePiAcceptance(t *testing.T) {
	configPath := strings.TrimSpace(os.Getenv("QM_REAL_LLM_CONFIG"))
	databaseURL := strings.TrimSpace(os.Getenv("QM_BACKEND_TEST_DATABASE_URL"))
	if configPath == "" || databaseURL == "" {
		t.Skip("QM_REAL_LLM_CONFIG and QM_BACKEND_TEST_DATABASE_URL are required")
	}
	configPath = liveLLMConfigPath(t, configPath)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load real LLM config: %v", err)
	}
	if override := strings.TrimSpace(os.Getenv("QM_REAL_LLM_MODEL")); override != "" {
		liveSelectPiModel(t, &cfg, override)
	}
	model := cfg.QM.Models.DefaultModel()
	provider, configured := cfg.QM.Models.ProviderForModel(model)
	if model == "" || !configured || provider.Protocol == "mock" || strings.TrimSpace(provider.APIKey) == "" {
		t.Fatal("QM_REAL_LLM_CONFIG must select a non-mock Pi model with credentials")
	}
	if _, ok := cfg.QM.Models.Harness("pi"); !ok {
		t.Fatal("QM_REAL_LLM_CONFIG must configure the pi harness")
	}
	// Live acceptance must remain bounded even when a developer's normal
	// configuration permits very long reasoning calls and retries.
	cfg.QM.Models.Request.TimeoutSeconds = 300
	cfg.QM.Models.Request.Retries = 0
	cfg.QM.Models.Request.OperationTimeoutSeconds = 310
	cfg.QM.Models.Request.DisableThinking = true
	cfg.QM.Models.Request.MaxOutputTokens = 8192

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pg, err := data.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('qm-backend-integration-tests'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('qm-backend-integration-tests'))")
	})
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE project_file_memberships,file_artifacts,knowledge_scopes,projects,durable_map_versions,deactivated_principals,source_auth_replay,directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,acl_grants,audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatal(err)
	}

	tempRoot := t.TempDir()
	cfg.Database.URL = databaseURL
	cfg.QM.OrgID = "real-llm-acceptance"
	cfg.QM.RouteMode = "go"
	cfg.QM.NodeCoreURL = "http://127.0.0.1:1"
	cfg.QM.AgentWorkspaceRoot = filepath.Join(tempRoot, "agent-workspaces")
	cfg.QM.FileStore.Mode = "local"
	cfg.QM.FileStore.LocalDir = filepath.Join(tempRoot, "files")
	cfg.QM.FileStore.TransferLocalDir = filepath.Join(tempRoot, "transfer")
	cfg.Knowledge.RootDir = filepath.Join(tempRoot, "knowledge")
	cfg.Auth.SourceSigningSecret = "real-llm-acceptance-source-secret"
	cfg.Auth.CapabilitySecret = "real-llm-acceptance-capability-secret"
	if err := os.MkdirAll(cfg.QM.FileStore.TransferLocalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	knowledgeStore, closeKnowledgeStore, err := openKnowledgeStore(ctx, pg, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeKnowledgeStore)
	knowledgeDB, err := sql.Open("pgx", withKnowledgeSearchPath(databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = knowledgeDB.Close() })
	if err := knowledgeDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	knowledgeAgent, err := buildKnowledgeAgent(cfg, knowledgeStore)
	if err != nil {
		t.Fatal(err)
	}

	members := []data.DirectoryMember{{PrincipalID: "alice", DisplayName: "Alice", Type: "internal"}}
	if err := data.NewDirectoryRepository(pg, cfg.QM.OrgID).Sync(ctx, data.DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	handler, err := qmserver.NewHTTPServer(cfg, pg, knowledgeAgent, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	project := liveCreateProject(t, handler, cfg.Auth.SourceSigningSecret)
	scopeID := biz.ProjectScopeID(project.ID)
	ref := knowlega.ScopeRef{OrgID: cfg.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: project.Name}

	evidencePacket := []byte(`# Helios Garden Evidence Packet — 2026-03-01

Mira Sol is the Operations Director of Helios Garden. During the Spring Renewal event, technician Rowan Pike applies Compost Blend A to Rose Bed North under Mira Sol's supervision. Rowan Pike reports to Mira Sol; Mira Sol owns operating-standard decisions, while Rowan Pike owns field implementation.

The authoritative Change Notice CN-26 says that, effective 2026-02-01, the current official brown-to-green ratio for Compost Blend A at Rose Bed North is 2:1. It explicitly retires the former 3:1 ratio.

The still-published Garden Quick Guide was last edited after CN-26 but incorrectly says that 3:1 remains the current official ratio. This is a confirmed stale claim and an unresolved maintenance defect. Preserve the stale evidence, represent Mira Sol and Rowan Pike as durable people, represent Compost Blend A as a durable concept, link the Spring Renewal event and reporting relationship, and create a contradiction or stale-claim review item rather than silently erasing the discrepancy.
`)
	uploads := []liveUploadResult{
		liveUploadProjectFile(t, handler, cfg, scopeID, "helios-evidence-packet.md", evidencePacket),
	}

	queuedStatus, err := knowledgeAgent.Status(ref)
	if err != nil || queuedStatus.State != agentservice.KnowledgeWorkspaceQueued || queuedStatus.SourceCount != 1 {
		t.Fatalf("queued status=%+v err=%v", queuedStatus, err)
	}
	for _, upload := range uploads {
		if upload.ProjectFile.Status != "queued" || upload.ProjectFile.QueueTaskID == nil || upload.ProjectFile.RawPath == nil {
			t.Fatalf("upload did not return queued membership: %+v", upload.ProjectFile)
		}
		rawPath := filepath.Join(queuedStatus.ProjectPath, filepath.FromSlash(*upload.ProjectFile.RawPath))
		info, statErr := os.Stat(rawPath)
		if statErr != nil || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("raw mirror must be immediately readable and immutable: path=%s info=%v err=%v", rawPath, info, statErr)
		}
		if _, readErr := knowledgeAgent.Read(ref, *upload.ProjectFile.RawPath); readErr != nil {
			t.Fatalf("read queued raw mirror %s: %v", *upload.ProjectFile.RawPath, readErr)
		}
	}
	queuedSearch, err := knowledgeAgent.Search(ctx, ref, "Helios Garden Compost Blend A", 20)
	if err != nil || !liveSearchHasPrefix(queuedSearch, "raw/") {
		t.Fatalf("queued raw search=%+v err=%v", queuedSearch, err)
	}

	projectFiles := data.NewProjectFileMembershipRepository(pg)
	knowledgeScopes := data.NewKnowledgeScopeRepository(pg)
	maintained, err := qmserver.ProcessProjectFileScope(ctx, project.ID, ref, projectFiles, knowledgeScopes, knowledgeAgent, true, false)
	if err != nil || maintained.Status != "ok" {
		t.Fatalf("real LLM project-file worker failed: status=%s err=%v steps=%+v", maintained.Status, err, maintained.Steps)
	}
	semanticIssues := liveSemanticReviewIssues(t, maintained)
	if !liveHasSemanticIssueType(semanticIssues, "contradiction", "stale-claim") {
		t.Fatalf("stale source claim produced no contradiction/stale-claim semantic issue: %+v", semanticIssues)
	}
	for _, upload := range uploads {
		membership, getErr := projectFiles.Get(ctx, project.ID, upload.File.ID)
		if getErr != nil || membership.Status != "ready" || membership.GeneratedPageCount == 0 {
			t.Fatalf("ready membership=%+v err=%v", membership, getErr)
		}
	}

	readyStatus, err := knowledgeAgent.Status(ref)
	if err != nil || readyStatus.State != agentservice.KnowledgeWorkspaceReady || readyStatus.SourceCount != 1 {
		t.Fatalf("ready status=%+v err=%v", readyStatus, err)
	}
	manifestValue, err := manifestfile.Load(readyStatus.ProjectPath)
	if err != nil || len(manifestValue.Sources) != 1 {
		t.Fatalf("source manifest entries=%d err=%v", len(manifestValue.Sources), err)
	}
	for _, entry := range manifestValue.Sources {
		if entry.QMFileID == "" || entry.QMProjectID != project.ID || entry.QMScopeID != scopeID || !liveContainsPrefix(entry.Files, "wiki/sources/") {
			t.Fatalf("manifest entry lacks QM provenance/source summary: %+v", entry)
		}
	}
	if sources := liveImmutableRawSourceCount(t, readyStatus.ProjectPath, manifestValue); sources != 1 {
		t.Fatalf("immutable manifest-backed raw sources=%d, want 1", sources)
	}

	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: readyStatus.ProjectPath, ProjectID: readyStatus.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	pageTypeCounts := map[string]int{}
	var candidate knowledgecore.WikiPage
	var generatedText strings.Builder
	for _, page := range pages {
		pageTypeCounts[page.Type]++
		generatedText.WriteString("\n" + page.Title + "\n" + page.Body)
		if page.Type == "entity" && strings.Contains(strings.ToLower(page.Title+"\n"+page.Body), "mira sol") {
			candidate = page
		}
	}
	if pageTypeCounts["source-summary"] != 1 || pageTypeCounts["entity"] == 0 || pageTypeCounts["concept"] == 0 {
		t.Fatalf("real LLM page types=%+v", pageTypeCounts)
	}
	if candidate.Path == "" {
		t.Fatalf("real LLM did not create a Mira Sol entity page: %+v", pageTypeCounts)
	}
	generatedLower := strings.ToLower(generatedText.String())
	for _, required := range []string{"mira sol", "rowan pike", "spring renewal", "compost blend a", "rose bed north"} {
		if !strings.Contains(generatedLower, required) {
			t.Fatalf("generated wiki lacks person/event/relationship term %q", required)
		}
	}
	if !strings.Contains(generatedLower, "2:1") && !strings.Contains(generatedLower, "2-to-1") && !strings.Contains(generatedLower, "2 to 1") {
		t.Fatal("generated wiki lacks the current 2:1 claim")
	}
	if !strings.Contains(candidate.Body, "[[") || !strings.Contains(strings.ToLower(candidate.Body), "rowan pike") {
		t.Fatalf("candidate entity lacks relationship wikilinks/evidence: %s", candidate.Body)
	}
	for _, rel := range []string{"wiki/index.md", "wiki/overview.md", "wiki/log.md", "wiki/reviews.md"} {
		content, readErr := os.ReadFile(filepath.Join(readyStatus.ProjectPath, filepath.FromSlash(rel)))
		if readErr != nil || len(bytes.TrimSpace(content)) == 0 {
			t.Fatalf("required wiki artifact %s is empty: %v", rel, readErr)
		}
	}
	overview, err := os.ReadFile(filepath.Join(readyStatus.ProjectPath, "wiki", "overview.md"))
	if err != nil || !bytes.Contains(overview, []byte("[[")) || bytes.Contains(overview, []byte("This page is maintained from ingested sources")) {
		t.Fatalf("overview was not synthesized: %s err=%v", overview, err)
	}
	reviews, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: readyStatus.ProjectPath, ProjectID: readyStatus.ProjectID})
	if err != nil || len(reviews) == 0 {
		t.Fatalf("wiki/reviews.md has no durable review item: reviews=%+v err=%v", reviews, err)
	}
	issues, err := agentservice.LintWiki(readyStatus.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		if issue.Type == "broken-link" {
			t.Fatalf("real LLM generated broken wikilink: %+v", issue)
		}
	}
	if liveKnowledgeRowCount(t, knowledgeDB, "source_manifest", readyStatus.ProjectID) != 1 || liveKnowledgeRowCount(t, knowledgeDB, "sources", readyStatus.ProjectID) != 1 {
		t.Fatal("PostgreSQL source provenance was not fully synchronized")
	}
	if got := liveKnowledgeRowCount(t, knowledgeDB, "wiki_pages", readyStatus.ProjectID); got != len(pages) {
		t.Fatalf("PostgreSQL wiki pages=%d filesystem=%d", got, len(pages))
	}
	if got := liveKnowledgeRowCount(t, knowledgeDB, "review_items", readyStatus.ProjectID); got != len(reviews) {
		t.Fatalf("PostgreSQL review items=%d markdown=%d", got, len(reviews))
	}
	if liveKnowledgeRowCount(t, knowledgeDB, "wiki_page_versions", readyStatus.ProjectID) == 0 {
		t.Fatal("real LLM maintenance preserved no page versions")
	}

	readySearch, err := knowledgeAgent.Search(ctx, ref, "Mira Sol Compost Blend A", 50)
	if err != nil || !liveSearchHasPrefix(readySearch, "raw/") || !liveSearchHasPrefix(readySearch, "wiki/sources/") || !liveSearchHasPrefix(readySearch, "wiki/entities/") {
		t.Fatalf("ready raw/source/entity recall=%+v err=%v", readySearch, err)
	}
	conceptSearch, err := knowledgeAgent.Search(ctx, ref, "Compost Blend A 2:1", 50)
	if err != nil || !liveSearchHasPrefix(conceptSearch, "wiki/concepts/") {
		t.Fatalf("ready concept recall=%+v err=%v", conceptSearch, err)
	}
	for _, result := range append(readySearch, conceptSearch...) {
		if strings.HasPrefix(result.Path, "raw/") || strings.HasPrefix(result.Path, "wiki/sources/") || strings.HasPrefix(result.Path, "wiki/entities/") || strings.HasPrefix(result.Path, "wiki/concepts/") {
			if _, readErr := knowledgeAgent.Read(ref, result.Path); readErr != nil {
				t.Fatalf("read recalled evidence %s: %v", result.Path, readErr)
			}
		}
	}

	piTrace := liveRunPiNineRequirementAcceptance(t, ctx, cfg, knowledgeAgent, ref, candidate)
	t.Logf("real LLM acceptance: model=%s sources=1 wiki_pages=%d entities=%d concepts=%d reviews=%d semantic_issues=%d pi_tool_calls=%d submit=%s", model, len(pages), pageTypeCounts["entity"], pageTypeCounts["concept"], len(reviews), len(semanticIssues), piTrace.ToolCalls, piTrace.SubmitStatus)
}

type liveUploadResult struct {
	File struct {
		ID string `json:"id"`
	} `json:"file"`
	ProjectFile data.ProjectFileMembership `json:"projectFile"`
}

func liveLLMConfigPath(t *testing.T, value string) string {
	t.Helper()
	if filepath.IsAbs(value) {
		return value
	}
	for _, candidate := range []string{value, filepath.Join("..", "..", value)} {
		if _, err := os.Stat(candidate); err == nil {
			absolute, absErr := filepath.Abs(candidate)
			if absErr != nil {
				t.Fatal(absErr)
			}
			return absolute
		}
	}
	t.Fatalf("QM_REAL_LLM_CONFIG does not exist: %s", value)
	return ""
}

func liveSelectPiModel(t *testing.T, cfg *config.Config, model string) {
	t.Helper()
	if cfg == nil {
		t.Fatal("real LLM config is nil")
	}
	for index := range cfg.QM.Models.Harnesses {
		harness := &cfg.QM.Models.Harnesses[index]
		if harness.ID != "pi" {
			continue
		}
		approved := false
		for _, candidate := range harness.ModelIDs {
			if candidate == model {
				approved = true
				break
			}
		}
		if !approved {
			t.Fatalf("QM_REAL_LLM_MODEL %q is not approved by the pi harness", model)
		}
		if _, ok := cfg.QM.Models.ProviderForModel(model); !ok {
			t.Fatalf("QM_REAL_LLM_MODEL %q has no configured provider", model)
		}
		harness.DefaultModel = model
		cfg.QM.Models.DefaultHarness = "pi"
		return
	}
	t.Fatal("QM_REAL_LLM_CONFIG must configure the pi harness")
}

func liveCreateProject(t *testing.T, handler http.Handler, secret string) biz.Project {
	t.Helper()
	body, err := json.Marshal(map[string]string{"principalId": "alice", "name": "Real LLM Knowledge Acceptance"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(body))
	liveSignSourceRequest(request, secret, body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", response.Code, response.Body.String())
	}
	var parsed struct {
		Project biz.Project `json:"project"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil || parsed.Project.ID == "" {
		t.Fatalf("create project response=%s err=%v", response.Body.String(), err)
	}
	return parsed.Project
}

func liveUploadProjectFile(t *testing.T, handler http.Handler, cfg config.Config, scopeID, name string, content []byte) liveUploadResult {
	t.Helper()
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	blobID, _, err := data.PutLocalTransferBlob(cfg.QM.FileStore.TransferLocalDir, bytes.NewReader(content), sha, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{
		"principalId": "alice", "blobId": blobID, "name": name, "mimetype": "text/markdown", "scopeId": scopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files/upload", bytes.NewReader(body))
	liveSignSourceRequest(request, cfg.Auth.SourceSigningSecret, body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload %s status=%d body=%s", name, response.Code, response.Body.String())
	}
	var parsed liveUploadResult
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil || parsed.File.ID == "" {
		t.Fatalf("upload %s response=%s err=%v", name, response.Body.String(), err)
	}
	return parsed
}

func liveSignSourceRequest(request *http.Request, secret string, body []byte) {
	timestamp := time.Now().Unix()
	canonical := request.Method + "\n" + request.URL.RequestURI() + "\n" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + fmt.Sprint(timestamp) + ":" + canonical))
	request.Header.Set("x-timestamp", fmt.Sprint(timestamp))
	request.Header.Set("x-signature", "v0="+fmt.Sprintf("%x", mac.Sum(nil)))
}

func liveSemanticReviewIssues(t *testing.T, result agentservice.MaintainWikiResult) []agentservice.LintIssue {
	t.Helper()
	for _, step := range result.Steps {
		if step.Name != "llm_review" {
			continue
		}
		if step.Status != "ok" {
			t.Fatalf("semantic review step=%+v", step)
		}
		encoded, err := json.Marshal(step.Detail)
		if err != nil {
			t.Fatal(err)
		}
		var detail struct {
			Issues []agentservice.LintIssue `json:"issues"`
		}
		if err := json.Unmarshal(encoded, &detail); err != nil {
			t.Fatalf("decode semantic review detail: %v detail=%s", err, encoded)
		}
		return detail.Issues
	}
	t.Fatal("maintenance result has no llm_review step")
	return nil
}

func liveHasSemanticIssueType(issues []agentservice.LintIssue, kinds ...string) bool {
	for _, issue := range issues {
		for _, kind := range kinds {
			if issue.Type == kind {
				return true
			}
		}
	}
	return false
}

func liveContainsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func liveImmutableRawSourceCount(t *testing.T, projectPath string, value manifestfile.File) int {
	t.Helper()
	count := 0
	for _, entry := range value.Sources {
		rel := filepath.ToSlash(strings.TrimSpace(entry.RawPath))
		if !strings.HasPrefix(rel, "raw/sources/") {
			t.Fatalf("manifest raw source is outside raw/sources: %q", rel)
		}
		path := filepath.Join(projectPath, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("manifest raw source is writable: %s mode=%o", path, info.Mode().Perm())
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		actualSHA := hex.EncodeToString(digest[:])
		if entry.SHA256 != actualSHA || entry.QMSourceSHA256 != actualSHA {
			t.Fatalf("manifest raw source hash mismatch: path=%s actual=%s entry=%s qm=%s", rel, actualSHA, entry.SHA256, entry.QMSourceSHA256)
		}
		count++
	}
	return count
}

func liveKnowledgeRowCount(t *testing.T, db *sql.DB, table, projectID string) int {
	t.Helper()
	allowed := map[string]bool{"wiki_pages": true, "wiki_page_versions": true, "source_manifest": true, "sources": true, "review_items": true}
	if !allowed[table] {
		t.Fatalf("unsupported knowledge table %q", table)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE project_id=$1", projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func liveSearchHasPrefix(results []knowledgecore.KnowledgeSearchResult, prefix string) bool {
	for _, result := range results {
		if strings.HasPrefix(result.Path, prefix) {
			return true
		}
	}
	return false
}

type livePiSession struct {
	sessionID string
	entries   []data.SessionEntry
}

func (s *livePiSession) Entries(_ context.Context, sessionID string, limit, since int) ([]data.SessionEntry, error) {
	var result []data.SessionEntry
	for _, entry := range s.entries {
		if entry.SessionID == sessionID && entry.Sequence >= since {
			result = append(result, entry)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result, nil
}

func (s *livePiSession) Emit(_ context.Context, entry qmagent.NewEntry) (qmagent.SessionEntry, error) {
	sequence := len(s.entries)
	createdAt := time.Now().UnixMilli()
	s.entries = append(s.entries, data.SessionEntry{
		SessionID: s.sessionID, Sequence: sequence, Type: entry.Type, Payload: append(json.RawMessage(nil), entry.Payload...), ScopeLabel: entry.ScopeLabel, CreatedAt: createdAt,
	})
	return qmagent.SessionEntry{SessionID: s.sessionID, Sequence: sequence, Type: entry.Type, Payload: entry.Payload, ScopeLabel: entry.ScopeLabel, CreatedAt: createdAt}, nil
}

type livePiTrace struct {
	ToolCalls    int
	SubmitStatus string
}

func liveRunPiNineRequirementAcceptance(t *testing.T, ctx context.Context, cfg config.Config, knowledgeAgent *knowlega.Agent, ref knowlega.ScopeRef, candidate knowledgecore.WikiPage) livePiTrace {
	t.Helper()
	requirements := []knowledgecore.KnowledgeRequirement{
		{ID: "r1", Text: "Mira Sol", Kind: "positive"},
		{ID: "r2", Text: "Operations Director", Kind: "positive"},
		{ID: "r3", Text: "Helios Garden", Kind: "positive"},
		{ID: "r4", Text: "Compost Blend A", Kind: "positive"},
		{ID: "r5", Text: "Rowan Pike", Kind: "positive"},
		{ID: "r6", Text: "Spring Renewal", Kind: "positive"},
		{ID: "r7", Text: "Rose Bed North", Kind: "positive"},
		{ID: "r8", Text: "2:1 ratio", Kind: "positive"},
		{ID: "r9", Text: "zqx-no-finance-director-91", Kind: "negative"},
	}
	prompt := fmt.Sprintf(`Run the project-knowledge acceptance protocol for candidate %q at path %q. Use only the knowledge tool for project facts; do not answer from this prompt.

Requirements are exactly:
- r1 positive: Mira Sol
- r2 positive: Operations Director
- r3 positive: Helios Garden
- r4 positive: Compost Blend A
- r5 positive: Rowan Pike
- r6 positive: Spring Renewal
- r7 positive: Rose Bed North
- r8 positive: 2:1 ratio
- r9 negative: zqx-no-finance-director-91

Required protocol:
1. Call knowledge action=search separately for every r1 through r9. Every search must set candidate=%q and its matching requirement_id. For r1-r8 use the requirement text as query. For r9 use only the exact query zqx-no-finance-director-91 so a zero result is meaningful.
2. Call knowledge action=read for path %q so the durable candidate entity page becomes evidence.
3. Call knowledge action=submit with the exact nine requirements and exactly one check per requirement. Mark r1-r8 supported with evidence_paths [%q]. Mark r9 not_found_in_corpus with no evidence path. Set question, answer, candidate=%q, and top-level evidence_paths [%q].
4. If submit is incomplete, correct the missing calls or fields and submit again. Finish only after submit returns complete, then give a short answer. Do not call writeback.`, candidate.Title, candidate.Path, candidate.Title, candidate.Path, candidate.Path, candidate.Title, candidate.Path)

	session := &livePiSession{sessionID: "real-llm-pi-acceptance"}
	userPayload, _ := json.Marshal(map[string]string{"text": prompt})
	session.entries = append(session.entries, data.SessionEntry{SessionID: session.sessionID, Sequence: 0, Type: "user", Payload: userPayload, ScopeLabel: ref.ExternalScopeID, CreatedAt: time.Now().UnixMilli()})
	turnStart := 0
	resolver, err := qmagent.NewCoreToolContextResolver(qmagent.CoreToolContextOptions{
		Sessions: session, Knowledge: knowledgeAgent, WorkspaceRoot: filepath.Join(cfg.QM.AgentWorkspaceRoot, "live-acceptance"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := resolver.ResolveToolContext(ctx, qmagent.TurnTaskPayload{
		SessionID: session.sessionID, ScopeLabel: ref.ExternalScopeID, OrgScopeID: "org:" + ref.OrgID, UserEntrySequence: &turnStart,
		Model: cfg.QM.Models.DefaultModel(), Harness: "pi",
	})
	if err != nil {
		t.Fatal(err)
	}
	input := qmagent.TurnInput{
		SessionID: session.sessionID, RunID: "real-llm-pi-run", Input: prompt,
		SystemPrompt: "You are QM's outer Pi reasoning agent. Follow the user's acceptance protocol exactly and use tool results as the only project evidence.",
		ScopeLabel:   ref.ExternalScopeID, OrgScopeID: "org:" + ref.OrgID, Model: cfg.QM.Models.DefaultModel(), Harness: "pi",
		TurnWallClockMS: 10 * 60 * 1000, UserEntrySequence: &turnStart, Tools: tools, Emit: session.Emit,
	}
	if _, err := qmagent.NewProjectKnowledgePreparer().PrepareTurnInput(ctx, &input); err != nil {
		t.Fatal(err)
	}
	adapter, err := qmagent.NewPiAdapter(cfg.QM.Models, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	turnResult, err := adapter.RunTurn(ctx, input)
	if err != nil {
		t.Fatalf("real Pi turn failed: %v", err)
	}
	if strings.TrimSpace(turnResult.Reply) == "" {
		t.Fatal("real Pi returned no final answer")
	}
	trace := liveInspectPiTrace(t, session.entries, candidate, requirements)
	if trace.SubmitStatus != "complete" {
		t.Fatalf("real Pi did not complete knowledge submit: trace=%+v reply=%s", trace, turnResult.Reply)
	}
	return trace
}

type livePiCall struct {
	Action        string `json:"action"`
	Query         string `json:"query"`
	Path          string `json:"path"`
	Candidate     string `json:"candidate"`
	RequirementID string `json:"requirement_id"`
}

func liveInspectPiTrace(t *testing.T, entries []data.SessionEntry, candidate knowledgecore.WikiPage, requirements []knowledgecore.KnowledgeRequirement) livePiTrace {
	t.Helper()
	calls := map[string]livePiCall{}
	searches := map[string]bool{}
	positiveResults := map[string]int{}
	readCandidate := false
	submitStatus := ""
	writeback := false
	toolCalls := 0
	for _, entry := range entries {
		var payload struct {
			Tool      string          `json:"tool"`
			CallID    string          `json:"callId"`
			Arguments json.RawMessage `json:"arguments"`
			Result    string          `json:"result"`
			IsError   bool            `json:"isError"`
		}
		if json.Unmarshal(entry.Payload, &payload) != nil || payload.Tool != "knowledge" {
			continue
		}
		switch entry.Type {
		case "tool_call":
			var call livePiCall
			if err := json.Unmarshal(payload.Arguments, &call); err != nil {
				t.Fatalf("decode Pi knowledge call: %v payload=%s", err, entry.Payload)
			}
			calls[payload.CallID] = call
			toolCalls++
			if call.Action == "search" {
				if call.RequirementID == "" || !strings.EqualFold(strings.TrimSpace(call.Candidate), strings.TrimSpace(candidate.Title)) {
					t.Fatalf("Pi search is not candidate-specific: %+v", call)
				}
				searches[call.RequirementID] = true
			}
			if call.Action == "read" && (strings.EqualFold(call.Path, candidate.Path) || strings.EqualFold(call.Path, candidate.Title)) {
				readCandidate = true
			}
			writeback = writeback || call.Action == "writeback"
		case "tool_result":
			call := calls[payload.CallID]
			if payload.IsError {
				t.Fatalf("Pi knowledge action %s failed: %s", call.Action, payload.Result)
			}
			if call.Action == "search" {
				var results []knowledgecore.KnowledgeSearchResult
				if err := json.Unmarshal([]byte(payload.Result), &results); err != nil {
					t.Fatalf("decode Pi search result for %s: %v result=%s", call.RequirementID, err, payload.Result)
				}
				positiveResults[call.RequirementID] = len(results)
			}
			if call.Action == "submit" {
				var submission knowledgecore.KnowledgeSubmission
				if err := json.Unmarshal([]byte(payload.Result), &submission); err != nil {
					t.Fatalf("decode Pi submit: %v result=%s", err, payload.Result)
				}
				submitStatus = submission.Status
			}
		}
	}
	for _, requirement := range requirements {
		if !searches[requirement.ID] {
			t.Fatalf("Pi did not run candidate-specific search for %s", requirement.ID)
		}
		if requirement.Kind == "positive" && positiveResults[requirement.ID] == 0 {
			t.Fatalf("Pi positive search %s returned no results", requirement.ID)
		}
		if requirement.Kind == "negative" && positiveResults[requirement.ID] != 0 {
			t.Fatalf("Pi negative search %s returned %d results", requirement.ID, positiveResults[requirement.ID])
		}
	}
	if !readCandidate {
		t.Fatalf("Pi did not read candidate entity page %s", candidate.Path)
	}
	if writeback {
		t.Fatal("Pi called writeback without an explicit user request")
	}
	return livePiTrace{ToolCalls: toolCalls, SubmitStatus: submitStatus}
}
