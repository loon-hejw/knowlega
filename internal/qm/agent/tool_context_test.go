package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	knowledgecore "github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	knowledgeservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type memoryToolFake struct{ content string }

func (f *memoryToolFake) Head(context.Context, string) (data.MemoryHead, error) {
	return data.MemoryHead{Content: f.content, Revision: "1"}, nil
}
func (f *memoryToolFake) Replace(_ context.Context, _ string, content, _ string) error {
	f.content = content
	return nil
}

type sessionToolFake struct{ entries []data.SessionEntry }

func (f sessionToolFake) Entries(_ context.Context, _ string, _ int, since int) ([]data.SessionEntry, error) {
	var result []data.SessionEntry
	for _, entry := range f.entries {
		if entry.Sequence >= since {
			result = append(result, entry)
		}
	}
	return result, nil
}

func knowledgeSearchEntry(sequence int, candidate, requirementID, query string, paths ...string) data.SessionEntry {
	sources := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		sources = append(sources, map[string]any{"path": path, "title": path, "kind": "wiki-page", "evidence": false})
	}
	details, _ := json.Marshal(map[string]any{
		"kind": "knowledge", "action": "search", "query": query, "candidate": candidate,
		"requirement_id": requirementID, "workspace_status": "ready", "sources": sources,
	})
	payload := toolResultEntryPayload(ToolCall{Name: "knowledge", ID: fmt.Sprintf("s-%d", sequence)}, toolTextWithDetails(`[]`, details))
	return data.SessionEntry{Sequence: sequence, Type: "tool_result", Payload: payload}
}

func knowledgeEvidenceReadEntry(sequence int, path, title string, aliases []string) data.SessionEntry {
	source := map[string]any{"path": path, "title": title, "kind": "wiki-page", "evidence": true}
	if len(aliases) > 0 {
		source["aliases"] = aliases
	}
	details, _ := json.Marshal(map[string]any{"kind": "knowledge", "action": "read", "sources": []any{source}})
	payload := toolResultEntryPayload(ToolCall{Name: "knowledge", ID: fmt.Sprintf("r-%d", sequence)}, toolTextWithDetails(`{}`, details))
	return data.SessionEntry{Sequence: sequence, Type: "tool_result", Payload: payload}
}

type deterministicKnowledgeFake struct{}

func (deterministicKnowledgeFake) Status(ref knowlega.ScopeRef) (knowlega.ScopeStatus, error) {
	return knowlega.ScopeStatus{Scope: ref, ProjectID: "p1", State: knowledgeservice.KnowledgeWorkspaceReady, Ready: true}, nil
}
func (deterministicKnowledgeFake) Search(context.Context, knowlega.ScopeRef, string, int) ([]knowledgecore.KnowledgeSearchResult, error) {
	return []knowledgecore.KnowledgeSearchResult{{Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"}}, nil
}
func (deterministicKnowledgeFake) Discover(context.Context, knowlega.ScopeRef, []knowledgecore.KnowledgeRequirement, int) ([]knowledgecore.KnowledgeCandidate, error) {
	return []knowledgecore.KnowledgeCandidate{{Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"}}, nil
}
func (deterministicKnowledgeFake) Read(knowlega.ScopeRef, string) (knowledgeservice.KnowledgeDocument, error) {
	return knowledgeservice.KnowledgeDocument{Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity", Content: "唐僧是取经团队成员。"}, nil
}
func (deterministicKnowledgeFake) List(knowlega.ScopeRef, string, int) ([]knowledgeservice.KnowledgePage, error) {
	return []knowledgeservice.KnowledgePage{{Path: "wiki/entities/tang.md", Title: "唐僧", Type: "entity"}}, nil
}
func (deterministicKnowledgeFake) FollowLinks(knowlega.ScopeRef, string, int) (knowledgeservice.KnowledgeFollowResult, error) {
	return knowledgeservice.KnowledgeFollowResult{Documents: []knowledgeservice.KnowledgeDocument{{Path: "raw/sources/ch1.md", Title: "第一章", Kind: "raw-source", Content: "原文"}}}, nil
}
func (deterministicKnowledgeFake) Graph(context.Context, knowlega.ScopeRef, string, []string, int) ([]knowledgeservice.KnowledgeDocument, error) {
	return []knowledgeservice.KnowledgeDocument{{Path: "raw/code-graphs/repo/graph.json#n1", Title: "Node", Kind: "code-graph", Content: "edge"}}, nil
}
func (deterministicKnowledgeFake) Writeback(context.Context, knowlega.ScopeRef, string, knowledgecore.KnowledgeSubmission) (knowledgeservice.KnowledgeWritebackResult, error) {
	return knowledgeservice.KnowledgeWritebackResult{Path: "wiki/syntheses/a.md", Written: true}, nil
}

type knowledgeStateFake struct {
	deterministicKnowledgeFake
	status  knowlega.ScopeStatus
	results []knowledgecore.KnowledgeSearchResult
}

func (f knowledgeStateFake) Status(ref knowlega.ScopeRef) (knowlega.ScopeStatus, error) {
	f.status.Scope = ref
	if f.status.ProjectID == "" {
		f.status.ProjectID = "p1"
	}
	return f.status, nil
}

func (f knowledgeStateFake) Search(context.Context, knowlega.ScopeRef, string, int) ([]knowledgecore.KnowledgeSearchResult, error) {
	return f.results, nil
}

type candidateSearchKnowledgeFake struct {
	deterministicKnowledgeFake
	query     string
	results   []knowledgecore.KnowledgeSearchResult
	documents map[string]knowledgeservice.KnowledgeDocument
}

func (f *candidateSearchKnowledgeFake) Search(_ context.Context, _ knowlega.ScopeRef, query string, _ int) ([]knowledgecore.KnowledgeSearchResult, error) {
	f.query = query
	return f.results, nil
}

func (f *candidateSearchKnowledgeFake) Read(_ knowlega.ScopeRef, path string) (knowledgeservice.KnowledgeDocument, error) {
	if document, ok := f.documents[path]; ok {
		return document, nil
	}
	return knowledgeservice.KnowledgeDocument{}, fmt.Errorf("document %s not found", path)
}

func TestCoreToolContextWorkspaceReadWriteAndConfinement(t *testing.T) {
	resolver, err := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "personal:alice", OrgScopeID: "org:acme"})
	if err != nil {
		t.Fatal(err)
	}
	written, err := tools.Execute(t.Context(), ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"notes/a.md","data":"hello"}`)})
	if err != nil || written.IsError {
		t.Fatalf("write=%+v err=%v", written, err)
	}
	read, err := tools.Execute(t.Context(), ToolCall{Name: "read", Arguments: json.RawMessage(`{"path":"notes/a.md"}`)})
	if err != nil || toolResultText(read) != "hello" {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	escape, _ := tools.Execute(t.Context(), ToolCall{Name: "write", Arguments: json.RawMessage(`{"path":"../../outside","data":"bad"}`)})
	if !escape.IsError || !strings.Contains(toolResultText(escape), "escapes the workspace") {
		t.Fatalf("escape=%+v", escape)
	}
}

func TestCoreToolContextExposesSingleKnowledgeTool(t *testing.T) {
	resolver, err := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: deterministicKnowledgeFake{}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := tools.Definitions(t.Context(), ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	if strings.Join(names, ",") != "memory,history,knowledge" {
		t.Fatalf("definitions=%v", names)
	}
}

func TestCoreToolContextHidesKnowledgeOutsideProjectScopes(t *testing.T) {
	resolver, err := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: deterministicKnowledgeFake{}})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "personal:alice", OrgScopeID: "org:acme"})
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := tools.Definitions(t.Context(), ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Name == "knowledge" {
			t.Fatalf("personal scope exposed Knowledge: %+v", definitions)
		}
	}
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"status"}`)})
	if err != nil || !result.IsError || !strings.Contains(toolResultText(result), `"code":"knowledge_unavailable"`) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestKnowledgeSearchIsNavigationAndReadIsEvidence(t *testing.T) {
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: deterministicKnowledgeFake{}})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	search, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"唐僧"}`)})
	if err != nil || search.IsError || !strings.Contains(string(search.Details), `"evidence":false`) {
		t.Fatalf("search=%+v err=%v", search, err)
	}
	read, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/entities/tang.md"}`)})
	if err != nil || read.IsError || !strings.Contains(string(read.Details), `"evidence":true`) {
		t.Fatalf("read=%+v err=%v", read, err)
	}
}

func TestKnowledgeReadCompactsLargeDocumentContentWithoutDroppingEvidenceMetadata(t *testing.T) {
	content := strings.Repeat("前段证据。", 5000) + "关键尾部证据。"
	fake := &largeDocumentKnowledgeFake{content: content}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: fake})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"read","path":"wiki/entities/large.md"}`)})
	if err != nil || result.IsError {
		t.Fatalf("read=%+v err=%v", result, err)
	}
	if len([]rune(toolResultText(result))) > knowledgeDocumentContentLimit+2000 || !strings.Contains(toolResultText(result), "关键尾部证据") || !strings.Contains(string(result.Details), `"evidence":true`) {
		t.Fatalf("large document was not compacted safely: bytes=%d result=%s details=%s", len(toolResultText(result)), toolResultText(result), result.Details)
	}
}

type largeDocumentKnowledgeFake struct {
	deterministicKnowledgeFake
	content string
}

func (f *largeDocumentKnowledgeFake) Read(knowlega.ScopeRef, string) (knowledgeservice.KnowledgeDocument, error) {
	return knowledgeservice.KnowledgeDocument{Path: "wiki/entities/large.md", Title: "Large", Kind: "entity", Content: f.content}, nil
}

func TestKnowledgeCandidateSearchRequiresCandidateAndRequirementIDTogether(t *testing.T) {
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: deterministicKnowledgeFake{}})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"唐僧 花果山","candidate":"唐僧"}`)})
	if err != nil || !result.IsError || !strings.Contains(toolResultText(result), "both `candidate` and `requirement_id`") {
		t.Fatalf("unpaired candidate search was accepted: result=%+v err=%v", result, err)
	}
}

func TestKnowledgeCandidateSearchUsesConditionQueryAndFiltersUnrelatedHits(t *testing.T) {
	fake := &candidateSearchKnowledgeFake{
		results: []knowledgecore.KnowledgeSearchResult{
			{Path: "raw/sources/monkey.md", Title: "花果山章节", Kind: "raw-source"},
			{Path: "raw/sources/heard.md", Title: "太宗听闻章节", Kind: "raw-source"},
			{Path: "raw/sources/mixed.md", Title: "长篇原文", Kind: "raw-source"},
			{Path: "raw/sources/taizong.md", Title: "太宗章节", Kind: "raw-source"},
		},
		documents: map[string]knowledgeservice.KnowledgeDocument{
			"唐太宗": {Path: "wiki/entities/tang-taizong.md", Title: "Emperor Taizong", Aliases: []string{"唐太宗", "唐王", "李世民"}, Sources: []string{
				"raw/sources/heard.md", "raw/sources/mixed.md", "raw/sources/taizong.md",
			}, Content: "唐太宗是大唐皇帝。"},
			"raw/sources/monkey.md":  {Path: "raw/sources/monkey.md", Title: "花果山章节", Kind: "raw-source", Content: "孙悟空住在花果山。"},
			"raw/sources/heard.md":   {Path: "raw/sources/heard.md", Title: "太宗听闻章节", Kind: "raw-source", Content: "太宗听说悟空来自花果山。"},
			"raw/sources/mixed.md":   {Path: "raw/sources/mixed.md", Title: "长篇原文", Kind: "raw-source", Content: "唐太宗在长安设宴。" + strings.Repeat("这是一段无关叙事。", 300) + "孙悟空到过花果山。"},
			"raw/sources/taizong.md": {Path: "raw/sources/taizong.md", Title: "太宗章节", Kind: "raw-source", Content: "唐太宗到过花果山。"},
		},
	}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: fake})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"到过 花果山","candidate":"唐太宗","requirement_id":"9"}`)})
	if err != nil || result.IsError || fake.query != "到过 花果山" {
		t.Fatalf("result=%+v query=%q err=%v", result, fake.query, err)
	}
	var matches []knowledgecore.KnowledgeSearchResult
	if err := json.Unmarshal([]byte(toolResultText(result)), &matches); err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Path != "raw/sources/taizong.md" || strings.Contains(toolResultText(result), "monkey.md") || strings.Contains(toolResultText(result), "heard.md") || strings.Contains(toolResultText(result), "mixed.md") {
		t.Fatalf("candidate filter retained unrelated results: %+v", matches)
	}
	if !strings.Contains(string(result.Details), `"query":"到过 花果山"`) || !strings.Contains(string(result.Details), `"candidate":"唐太宗"`) {
		t.Fatalf("candidate search metadata=%s", result.Details)
	}
}

func TestKnowledgeStatusAndEmptySearchUseRealWorkspaceState(t *testing.T) {
	fake := knowledgeStateFake{status: knowlega.ScopeStatus{State: knowledgeservice.KnowledgeWorkspaceEmpty, Queue: knowledgeservice.WorkspaceQueueStatus{}}}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: fake})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	status, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"status"}`)})
	if err != nil || status.IsError || !strings.Contains(toolResultText(status), `"status":"empty"`) || strings.Contains(toolResultText(status), `"status":"active"`) {
		t.Fatalf("status=%+v err=%v text=%s", status, err, toolResultText(status))
	}
	search, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"missing"}`)})
	if err != nil || !search.IsError || !strings.Contains(toolResultText(search), `"code":"knowledge_empty"`) {
		t.Fatalf("search=%+v err=%v text=%s", search, err, toolResultText(search))
	}
}

func TestKnowledgeSearchReportsPendingAndFailedWorkspace(t *testing.T) {
	for _, test := range []struct {
		name, state, code string
		isError           bool
	}{
		{name: "queued", state: knowledgeservice.KnowledgeWorkspaceQueued, code: `"status":"pending"`},
		{name: "processing", state: knowledgeservice.KnowledgeWorkspaceProcessing, code: `"status":"pending"`},
		{name: "failed", state: knowledgeservice.KnowledgeWorkspaceFailed, code: `"code":"knowledge_failed"`, isError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := knowledgeStateFake{status: knowlega.ScopeStatus{State: test.state, LastError: "provider unavailable"}}
			resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Knowledge: fake})
			tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
			result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: json.RawMessage(`{"action":"search","query":"missing"}`)})
			if err != nil || result.IsError != test.isError || !strings.Contains(toolResultText(result), test.code) {
				t.Fatalf("result=%+v err=%v text=%s", result, err, toolResultText(result))
			}
			if !test.isError && !strings.Contains(string(result.Details), `"workspace_status":"`+test.state+`"`) {
				t.Fatalf("pending state missing from ledger details: %s", result.Details)
			}
		})
	}
}

func TestKnowledgeSubmitRejectsUnreadAndAcceptsCurrentTurnRead(t *testing.T) {
	sessions := sessionToolFake{entries: []data.SessionEntry{
		knowledgeEvidenceReadEntry(9, "wiki/entities/tang.md", "唐僧", nil),
		knowledgeSearchEntry(11, "唐僧", "1", "取经成员", "wiki/entities/tang.md"),
		knowledgeEvidenceReadEntry(12, "wiki/entities/tang.md", "唐僧", nil),
	}}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessions, Knowledge: deterministicKnowledgeFake{}})
	seq := 10
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", UserEntrySequence: &seq})
	args := json.RawMessage(`{"action":"submit","question":"谁？","answer":"唐僧","candidate":"唐僧","requirements":[{"id":"1","text":"取经成员"}],"checks":[{"requirement_id":"1","status":"supported","evidence_paths":["wiki/entities/tang.md"]}],"evidence_paths":["wiki/entities/tang.md"]}`)
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || result.IsError || !strings.Contains(toolResultText(result), `"status":"complete"`) {
		t.Fatalf("result=%+v err=%v text=%s", result, err, toolResultText(result))
	}

	resolver, _ = NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessionToolFake{entries: sessions.entries[:1]}, Knowledge: deterministicKnowledgeFake{}})
	tools, _ = resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", UserEntrySequence: &seq})
	result, err = tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || !strings.Contains(toolResultText(result), `"status":"incomplete"`) {
		t.Fatalf("old-turn evidence accepted: %+v err=%v", result, err)
	}
}

func TestKnowledgeSubmitAcceptsEvidenceAfterSequenceZeroUserEntry(t *testing.T) {
	sessions := sessionToolFake{entries: []data.SessionEntry{
		{Sequence: 0, Type: "user", Payload: json.RawMessage(`{"text":"谁？"}`)},
		knowledgeSearchEntry(1, "唐僧", "1", "取经成员", "wiki/entities/tang.md"),
		knowledgeEvidenceReadEntry(2, "wiki/entities/tang.md", "唐僧", nil),
	}}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessions, Knowledge: deterministicKnowledgeFake{}})
	seq := 0
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", UserEntrySequence: &seq})
	args := json.RawMessage(`{"action":"submit","question":"谁？","answer":"唐僧","candidate":"唐僧","requirements":[{"id":"1","text":"取经成员"}],"checks":[{"requirement_id":"1","status":"supported","evidence_paths":["wiki/entities/tang.md"]}],"evidence_paths":["wiki/entities/tang.md"]}`)
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || result.IsError || !strings.Contains(toolResultText(result), `"status":"complete"`) {
		t.Fatalf("sequence-zero evidence was not accepted: result=%+v err=%v text=%s", result, err, toolResultText(result))
	}
}

func TestKnowledgeSubmitAcceptsNineConditionDecoratedCandidateAliasFromReadLedger(t *testing.T) {
	sessions := sessionToolFake{entries: []data.SessionEntry{
		knowledgeSearchEntry(11, "唐太宗", "1", "有结义的情节", "wiki/entities/yudi-shengseng.md"),
		knowledgeEvidenceReadEntry(12, "wiki/entities/yudi-shengseng.md", "Yudi Shengseng", nil),
		knowledgeSearchEntry(13, "唐太宗", "2", "见过孙悟空", "wiki/sources/chapter-100.md"),
		knowledgeEvidenceReadEntry(14, "wiki/sources/chapter-100.md", "第一百回　径回东土　五圣成真", nil),
		knowledgeSearchEntry(15, "唐太宗", "3", "跟孙悟空不算敌对关系", "wiki/entities/tang-taizong.md"),
		knowledgeEvidenceReadEntry(16, "wiki/entities/tang-taizong.md", "Emperor Taizong", []string{"Tang Taizong", "唐太宗", "唐王", "太宗", "李世民"}),
		knowledgeSearchEntry(17, "唐太宗", "4", "出场不止一次", "wiki/entities/tang-taizong.md"),
		knowledgeEvidenceReadEntry(18, "wiki/entities/tang-taizong.md", "Emperor Taizong", []string{"Tang Taizong", "唐太宗", "唐王", "太宗", "李世民"}),
		knowledgeSearchEntry(19, "唐太宗", "5", "曾逼迫唐僧做了某事", "wiki/sources/chapter-012.md"),
		knowledgeEvidenceReadEntry(20, "wiki/sources/chapter-012.md", "第十二回　玄奘秉诚建大会　观音显象化金蝉", nil),
		knowledgeSearchEntry(21, "唐太宗", "6", "最后唐僧就范而且没受到什么损失", "wiki/sources/chapter-012.md"),
		knowledgeEvidenceReadEntry(22, "wiki/sources/chapter-012.md", "第十二回　玄奘秉诚建大会　观音显象化金蝉", nil),
		knowledgeSearchEntry(23, "唐太宗", "7", "见过阎罗王", "wiki/sources/chapter-010.md"),
		knowledgeEvidenceReadEntry(24, "wiki/sources/chapter-010.md", "第十回　二将军宫门镇鬼　唐太宗地府还魂", nil),
		knowledgeSearchEntry(25, "唐太宗", "8", "见过观音", "wiki/sources/chapter-010.md"),
		knowledgeEvidenceReadEntry(26, "wiki/sources/chapter-010.md", "第十回　二将军宫门镇鬼　唐太宗地府还魂", nil),
		knowledgeSearchEntry(27, "唐太宗", "9", "到过 花果山"),
	}}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessions, Knowledge: deterministicKnowledgeFake{}})
	seq := 10
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", UserEntrySequence: &seq})
	requirements := []knowledgecore.KnowledgeRequirement{
		{ID: "1", Text: "有结义的情节", Kind: "positive"},
		{ID: "2", Text: "见过孙悟空", Kind: "positive"},
		{ID: "3", Text: "跟孙悟空不算敌对关系", Kind: "positive"},
		{ID: "4", Text: "出场不止一次", Kind: "positive"},
		{ID: "5", Text: "曾逼迫唐僧做了某事", Kind: "positive"},
		{ID: "6", Text: "最后唐僧就范,而且没受到什么损失", Kind: "positive"},
		{ID: "7", Text: "见过阎罗王", Kind: "positive"},
		{ID: "8", Text: "见过观音", Kind: "positive"},
		{ID: "9", Text: "不曾到过花果山", Kind: "negative"},
	}
	checks := []knowledgecore.KnowledgeEvidenceCheck{
		{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/yudi-shengseng.md"}},
		{RequirementID: "2", Status: "supported", EvidencePaths: []string{"wiki/sources/chapter-100.md"}},
		{RequirementID: "3", Status: "supported", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}},
		{RequirementID: "4", Status: "supported", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}},
		{RequirementID: "5", Status: "supported", EvidencePaths: []string{"wiki/sources/chapter-012.md"}},
		{RequirementID: "6", Status: "supported", EvidencePaths: []string{"wiki/sources/chapter-012.md"}},
		{RequirementID: "7", Status: "supported", EvidencePaths: []string{"wiki/sources/chapter-010.md"}},
		{RequirementID: "8", Status: "supported", EvidencePaths: []string{"wiki/sources/chapter-010.md"}},
		{RequirementID: "9", Status: "not_found_in_corpus", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}},
	}
	args, _ := json.Marshal(map[string]any{
		"action": "submit", "question": "1.有结义的情节 2.见过孙悟空 3.跟孙悟空不算敌对关系 4.出场不止一次 5.曾逼迫唐僧做了某事 6.最后唐僧就范,而且没受到什么损失 7.见过阎罗王 8.见过观音 9.不曾到过花果山",
		"answer": "唐太宗（唐王李世民）", "candidate": "唐太宗（唐王李世民）", "requirements": requirements, "checks": checks,
	})
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	var submission knowledgecore.KnowledgeSubmission
	decodeErr := json.Unmarshal([]byte(toolResultText(result)), &submission)
	if err != nil || result.IsError || decodeErr != nil || submission.Status != "complete" || len(submission.UnresolvedRequirementIDs) != 0 || len(submission.Citations) != 5 {
		t.Fatalf("nine-condition candidate alias was not accepted: submission=%+v result=%+v err=%v decodeErr=%v text=%s", submission, result, err, decodeErr, toolResultText(result))
	}
	if !containsString(submission.Citations[0].Aliases, "唐太宗") && !containsString(submission.Citations[1].Aliases, "唐太宗") && !containsString(submission.Citations[2].Aliases, "唐太宗") && !containsString(submission.Citations[3].Aliases, "唐太宗") && !containsString(submission.Citations[4].Aliases, "唐太宗") {
		t.Fatalf("candidate aliases missing from citations: %+v", submission.Citations)
	}
}

func TestKnowledgeSubmitReportsMissingFieldsAndNegativeSearchProtocol(t *testing.T) {
	ledger := knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang-taizong.md": {
			Path: "wiki/entities/tang-taizong.md", Title: "Emperor Taizong", Kind: "wiki-page",
			Aliases: []string{"唐太宗", "唐王", "李世民"},
		},
	}, Searches: []knowledgecore.KnowledgeActionRecord{{
		Action: "search", Query: "到过 花果山", ResultCount: 0, WorkspaceStatus: knowledgeservice.KnowledgeWorkspaceReady,
	}}}
	submission := knowledgecore.KnowledgeSubmission{
		Candidate: "唐太宗（唐王李世民）",
		Requirements: []knowledgecore.KnowledgeRequirement{
			{ID: "1", Text: "出场不止一次", Kind: "positive"},
			{ID: "9", Text: "不曾到过花果山", Kind: "negative"},
		},
		Checks: []knowledgecore.KnowledgeEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}},
			{RequirementID: "9", Status: "not_found_in_corpus", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}},
		},
	}

	validated := validateKnowledgeSubmission(submission, ledger)
	if validated.Status != "incomplete" || !hasValidationIssue(validated.ValidationIssues, "missing_question", "") || !hasValidationIssue(validated.ValidationIssues, "missing_answer", "") {
		t.Fatalf("missing submit fields were not explained: %+v", validated)
	}
	if hasValidationIssue(validated.ValidationIssues, "candidate_page_not_read", "") {
		t.Fatalf("decorated candidate alias did not match the read page: %+v", validated.ValidationIssues)
	}
	if !hasValidationIssue(validated.ValidationIssues, "negative_search_not_recorded", "9") {
		t.Fatalf("untagged negative search was not explained: %+v", validated.ValidationIssues)
	}
}

func TestKnowledgeSubmitReportsRequiredCandidateRequirementsAndChecks(t *testing.T) {
	validated := validateKnowledgeSubmission(knowledgecore.KnowledgeSubmission{
		Question: "谁满足条件？",
		Answer:   "未知",
	}, knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{}})
	for _, code := range []string{"missing_candidate", "missing_requirements", "missing_checks"} {
		if !hasValidationIssue(validated.ValidationIssues, code, "") {
			t.Fatalf("missing %s validation issue: %+v", code, validated)
		}
	}
	if validated.Status != "incomplete" {
		t.Fatalf("malformed submit completed: %+v", validated)
	}
}

func TestKnowledgePositiveSupportedAcceptsCurrentTurnEvidenceInAnyOrder(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐僧", Candidate: "唐僧",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "1", Text: "见过孙悟空", Kind: "positive"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"raw/sources/chapter-001.txt"}}},
		EvidencePaths: []string{"raw/sources/chapter-001.txt"},
	}
	evidence := map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang.md":       {Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"},
		"raw/sources/chapter-001.txt": {Path: "raw/sources/chapter-001.txt", Title: "第一回", Kind: "raw-source"},
	}
	withoutSearch := validateKnowledgeSubmission(base, knowledgeLedger{
		Evidence: evidence, EvidenceSequence: map[string]int{"wiki/entities/tang.md": 1, "raw/sources/chapter-001.txt": 2},
	})
	if withoutSearch.Status != "complete" {
		t.Fatalf("current-turn read should not need a matching search: %+v", withoutSearch)
	}

	search := knowledgecore.KnowledgeActionRecord{
		Action: "search", Query: "见过孙悟空", Candidate: "唐僧", RequirementID: "1", ResultCount: 1, Sequence: 3,
		ResultPaths: []string{"raw/sources/chapter-001.txt"},
	}
	readTooEarly := validateKnowledgeSubmission(base, knowledgeLedger{
		Evidence: evidence, EvidenceSequence: map[string]int{"wiki/entities/tang.md": 1, "raw/sources/chapter-001.txt": 2}, Searches: []knowledgecore.KnowledgeActionRecord{search},
	})
	if readTooEarly.Status != "complete" {
		t.Fatalf("pre-search read should satisfy positive check: %+v", readTooEarly)
	}

	validated := validateKnowledgeSubmission(base, knowledgeLedger{
		Evidence: evidence, EvidenceSequence: map[string]int{"wiki/entities/tang.md": 1, "raw/sources/chapter-001.txt": 4}, Searches: []knowledgecore.KnowledgeActionRecord{search},
	})
	if validated.Status != "complete" || len(validated.UnresolvedRequirementIDs) != 0 {
		t.Fatalf("matching search followed by read was rejected: %+v", validated)
	}
}

func TestKnowledgePositiveSupportedAcceptsParaphrasedEventSearch(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁有结义情节？", Answer: "唐太宗", Candidate: "唐太宗",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "1", Text: "有结义的情节", Kind: "positive"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "1", Status: "supported", EvidencePaths: []string{"raw/sources/chapter-012.txt"}}},
		EvidencePaths: []string{"raw/sources/chapter-012.txt"},
	}
	ledger := knowledgeLedger{
		Evidence: map[string]knowledgecore.KnowledgeCitation{
			"wiki/entities/tang-taizong.md": {Path: "wiki/entities/tang-taizong.md", Title: "Emperor Taizong", Aliases: []string{"唐太宗"}},
			"raw/sources/chapter-012.txt":   {Path: "raw/sources/chapter-012.txt", Title: "chapter-012", Kind: "raw-source"},
		},
		EvidenceSequence: map[string]int{
			"wiki/entities/tang-taizong.md": 1,
			"raw/sources/chapter-012.txt":   3,
		},
		Searches: []knowledgecore.KnowledgeActionRecord{{
			Action: "search", Query: "拜为兄弟", Candidate: "Emperor Taizong", RequirementID: "1",
			ResultCount: 1, Sequence: 2, ResultPaths: []string{"raw/sources/chapter-012.txt"},
		}},
	}
	validated := validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" || len(validated.UnresolvedRequirementIDs) != 0 {
		t.Fatalf("paraphrased positive search was rejected: %+v", validated)
	}
}

func TestKnowledgeNegativeNotFoundRequiresCandidateSearchWithRequirementID(t *testing.T) {
	readDetails := json.RawMessage(`{"kind":"knowledge","action":"read","sources":[{"path":"wiki/entities/tang.md","title":"唐僧","kind":"entity","evidence":true}]}`)
	searchDetails := json.RawMessage(`{"kind":"knowledge","action":"search","query":"到过 花果山","candidate":"唐僧","requirement_id":"neg","workspace_status":"ready","sources":[]}`)
	entries := []data.SessionEntry{
		{Sequence: 11, Type: "tool_result", Payload: toolResultEntryPayload(ToolCall{Name: "knowledge"}, toolTextWithDetails(`{}`, readDetails))},
		{Sequence: 12, Type: "tool_result", Payload: toolResultEntryPayload(ToolCall{Name: "knowledge"}, toolTextWithDetails(`[]`, searchDetails))},
	}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessionToolFake{entries: entries}, Knowledge: deterministicKnowledgeFake{}})
	seq := 10
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme", UserEntrySequence: &seq})
	args := json.RawMessage(`{"action":"submit","question":"谁？","answer":"唐僧","candidate":"唐僧","requirements":[{"id":"neg","text":"未到过花果山","kind":"negative"}],"checks":[{"requirement_id":"neg","status":"not_found_in_corpus"}],"evidence_paths":["wiki/entities/tang.md"]}`)
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || !strings.Contains(toolResultText(result), `"status":"complete"`) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestKnowledgeNegativeNotFoundAcceptsCandidatePageTitleAliasInSearch(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐太宗", Candidate: "唐太宗（唐王李世民）",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "9", Text: "不曾到过花果山", Kind: "negative"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "9", Status: "not_found_in_corpus"}},
		EvidencePaths: []string{"wiki/entities/tang-taizong.md"},
	}
	ledger := knowledgeLedger{
		Evidence: map[string]knowledgecore.KnowledgeCitation{
			"wiki/entities/tang-taizong.md": {
				Path: "wiki/entities/tang-taizong.md", Title: "Emperor Taizong", Kind: "entity",
				Aliases: []string{"唐太宗", "唐王", "李世民"},
			},
		},
		Searches: []knowledgecore.KnowledgeActionRecord{{
			Action: "search", Query: "到过 花果山", Candidate: "Emperor Taizong", RequirementID: "9",
			ResultCount: 0, WorkspaceStatus: knowledgeservice.KnowledgeWorkspaceReady,
		}},
	}
	validated := validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" || len(validated.UnresolvedRequirementIDs) != 0 {
		t.Fatalf("title/alias search was not accepted: %+v", validated)
	}
}

func TestKnowledgeNegativeNotFoundReturnsRepairAction(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐太宗", Candidate: "唐太宗（唐王李世民）",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "9", Text: "不曾到过花果山", Kind: "negative"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "9", Status: "not_found_in_corpus"}},
		EvidencePaths: []string{"wiki/entities/tang-taizong.md"},
	}
	validated := validateKnowledgeSubmission(base, knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang-taizong.md": {Path: "wiki/entities/tang-taizong.md", Title: "Emperor Taizong", Kind: "wiki-page", Aliases: []string{"唐太宗"}},
	}})
	if validated.Status != "incomplete" || len(validated.ValidationIssues) != 1 {
		t.Fatalf("validated=%+v", validated)
	}
	issue := validated.ValidationIssues[0]
	if issue.Repair == nil || issue.Repair.Action != "search" || issue.Repair.Candidate != "唐太宗" || issue.Repair.RequirementID != "9" || issue.Repair.Query != "到过花果山" {
		t.Fatalf("repair=%+v", issue.Repair)
	}
}

func TestKnowledgeNegativeRequirementAcceptsExplicitEvidence(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐太宗", Candidate: "唐太宗",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "9", Text: "不曾到过花果山", Kind: "negative"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "9", Status: "supported", Explanation: "原文明确陈述他未到过花果山", EvidencePaths: []string{"wiki/entities/tang-taizong.md"}}},
		EvidencePaths: []string{"wiki/entities/tang-taizong.md"},
	}
	ledger := knowledgeLedger{
		Evidence: map[string]knowledgecore.KnowledgeCitation{
			"wiki/entities/tang-taizong.md": {Path: "wiki/entities/tang-taizong.md", Title: "唐太宗", Kind: "entity"},
		},
	}
	validated := validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" {
		t.Fatalf("explicit negative evidence should not need an absence search: %+v", validated)
	}
}

func TestKnowledgeNegativeNotFoundAllowsPiSelectedParaphrase(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐太宗", Candidate: "唐太宗",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "9", Text: "不曾到过花果山", Kind: "negative"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "9", Status: "not_found_in_corpus"}},
		EvidencePaths: []string{"wiki/entities/tang-taizong.md"},
	}
	ledger := knowledgeLedger{
		Evidence: map[string]knowledgecore.KnowledgeCitation{
			"wiki/entities/tang-taizong.md": {Path: "wiki/entities/tang-taizong.md", Title: "唐太宗", Kind: "entity"},
		},
		Searches: []knowledgecore.KnowledgeActionRecord{{
			Action: "search", Query: "踏足水帘洞", Candidate: "唐太宗", RequirementID: "9",
			ResultCount: 0, WorkspaceStatus: knowledgeservice.KnowledgeWorkspaceReady,
		}},
	}
	validated := validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" {
		t.Fatalf("Pi-selected paraphrase was rejected: %+v", validated)
	}
}

func TestKnowledgeNegativeNotFoundRejectsHitsAndPendingSearches(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐僧", Candidate: "唐僧",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "neg", Text: "未到过花果山", Kind: "negative"}},
		Checks:        []knowledgecore.KnowledgeEvidenceCheck{{RequirementID: "neg", Status: "not_found_in_corpus"}},
		EvidencePaths: []string{"wiki/entities/tang.md"},
	}
	evidence := map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang.md": {Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"},
	}
	for _, record := range []knowledgecore.KnowledgeActionRecord{
		{Action: "search", Query: "到过 花果山", Candidate: "唐僧", RequirementID: "neg", ResultCount: 1, WorkspaceStatus: knowledgeservice.KnowledgeWorkspaceReady},
		{Action: "search", Query: "到过 花果山", Candidate: "唐僧", RequirementID: "neg", ResultCount: 0, WorkspaceStatus: knowledgeservice.KnowledgeWorkspaceProcessing},
		{Action: "search", Query: "到过 花果山", Candidate: "唐僧", RequirementID: "neg", ResultCount: 0},
	} {
		validated := validateKnowledgeSubmission(base, knowledgeLedger{Evidence: evidence, Searches: []knowledgecore.KnowledgeActionRecord{record}})
		if validated.Status != "incomplete" || !containsString(validated.UnresolvedRequirementIDs, "neg") {
			t.Fatalf("invalid negative search accepted: record=%+v submission=%+v", record, validated)
		}
		if record.ResultCount > 0 {
			if !hasValidationIssue(validated.ValidationIssues, "negative_search_found_results", "neg") {
				t.Fatalf("search hit was not explained: %+v", validated.ValidationIssues)
			}
		} else if !hasValidationIssue(validated.ValidationIssues, "negative_search_not_recorded", "neg") {
			t.Fatalf("pending search was not explained: %+v", validated.ValidationIssues)
		}
	}
}

func TestKnowledgeSubmitWithoutExplicitTurnSequenceUsesLatestUserBoundary(t *testing.T) {
	entries := []data.SessionEntry{
		knowledgeEvidenceReadEntry(4, "wiki/entities/tang.md", "唐僧", nil),
		{Sequence: 10, Type: "user", Payload: json.RawMessage(`{"text":"new turn"}`)},
	}
	resolver, _ := NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessionToolFake{entries: entries}, Knowledge: deterministicKnowledgeFake{}})
	tools, _ := resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	args := json.RawMessage(`{"action":"submit","question":"谁？","answer":"唐僧","candidate":"唐僧","requirements":[{"id":"1","text":"取经成员"}],"checks":[{"requirement_id":"1","status":"supported","evidence_paths":["wiki/entities/tang.md"]}],"evidence_paths":["wiki/entities/tang.md"]}`)
	result, err := tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || !strings.Contains(toolResultText(result), `"status":"incomplete"`) {
		t.Fatalf("old evidence crossed the latest user boundary: result=%+v err=%v", result, err)
	}

	entries = append(entries,
		knowledgeSearchEntry(11, "唐僧", "1", "取经成员", "wiki/entities/tang.md"),
		knowledgeEvidenceReadEntry(12, "wiki/entities/tang.md", "唐僧", nil),
	)
	resolver, _ = NewCoreToolContextResolver(CoreToolContextOptions{WorkspaceRoot: t.TempDir(), Sessions: sessionToolFake{entries: entries}, Knowledge: deterministicKnowledgeFake{}})
	tools, _ = resolver.ResolveToolContext(t.Context(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "group:web-project-p1", OrgScopeID: "org:acme"})
	result, err = tools.Execute(t.Context(), ToolCall{Name: "knowledge", Arguments: args})
	if err != nil || !strings.Contains(toolResultText(result), `"status":"complete"`) {
		t.Fatalf("current-turn evidence was not accepted: result=%+v err=%v", result, err)
	}
}

func TestKnowledgeSubmitRejectsDuplicateAndUnknownChecks(t *testing.T) {
	ledger := knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang.md": {Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"},
	}}
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐僧", Candidate: "唐僧",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "1", Text: "取经成员"}},
		EvidencePaths: []string{"wiki/entities/tang.md"},
	}

	duplicate := base
	duplicate.Checks = []knowledgecore.KnowledgeEvidenceCheck{
		{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
		{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
	}
	validated := validateKnowledgeSubmission(duplicate, ledger)
	if validated.Status != "incomplete" || !containsString(validated.UnresolvedRequirementIDs, "1") {
		t.Fatalf("duplicate checks accepted: %+v", validated)
	}

	unknown := base
	unknown.Checks = []knowledgecore.KnowledgeEvidenceCheck{
		{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
		{RequirementID: "unknown", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
	}
	validated = validateKnowledgeSubmission(unknown, ledger)
	if validated.Status != "incomplete" || !containsString(validated.UnresolvedRequirementIDs, "unknown") {
		t.Fatalf("unknown check accepted: %+v", validated)
	}
}

func TestKnowledgeSubmitRejectsMalformedRequirementAndCheckIDs(t *testing.T) {
	ledger := knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{
		"wiki/entities/tang.md": {Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"},
	}}

	malformedRequirement := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐僧", Candidate: "唐僧",
		Requirements:  []knowledgecore.KnowledgeRequirement{{ID: "", Text: "取经成员"}},
		EvidencePaths: []string{"wiki/entities/tang.md"},
	}
	if validated := validateKnowledgeSubmission(malformedRequirement, ledger); validated.Status != "incomplete" {
		t.Fatalf("empty requirement id accepted: %+v", validated)
	}

	malformedCheck := knowledgecore.KnowledgeSubmission{
		Question: "谁？", Answer: "唐僧", Candidate: "唐僧",
		Requirements: []knowledgecore.KnowledgeRequirement{{ID: "1", Text: "取经成员"}},
		Checks: []knowledgecore.KnowledgeEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
			{RequirementID: "", Status: "supported", EvidencePaths: []string{"wiki/entities/tang.md"}},
		},
		EvidencePaths: []string{"wiki/entities/tang.md"},
	}
	if validated := validateKnowledgeSubmission(malformedCheck, ledger); validated.Status != "incomplete" {
		t.Fatalf("empty check id accepted: %+v", validated)
	}
}

func TestKnowledgeSubmitMultiConditionAcceptsSharedRawEvidence(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{
		Question: "谁符合？", Answer: "唐僧", Candidate: "唐僧",
		Requirements: []knowledgecore.KnowledgeRequirement{{ID: "1", Text: "取经成员"}, {ID: "2", Text: "师父"}},
		Checks: []knowledgecore.KnowledgeEvidenceCheck{
			{RequirementID: "1", Status: "supported", EvidencePaths: []string{"raw/sources/ch1.md"}},
			{RequirementID: "2", Status: "supported", EvidencePaths: []string{"raw/sources/ch1.md"}},
		},
		EvidencePaths: []string{"raw/sources/ch1.md"},
	}
	ledger := knowledgeLedger{
		Evidence: map[string]knowledgecore.KnowledgeCitation{
			"raw/sources/ch1.md": {Path: "raw/sources/ch1.md", Title: "唐僧", Kind: "raw-source"},
		},
		EvidenceSequence: map[string]int{"raw/sources/ch1.md": 3},
		Searches: []knowledgecore.KnowledgeActionRecord{
			{Action: "search", Query: "取经成员", Candidate: "唐僧", RequirementID: "1", ResultCount: 1, Sequence: 1, ResultPaths: []string{"raw/sources/ch1.md"}},
			{Action: "search", Query: "师父", Candidate: "唐僧", RequirementID: "2", ResultCount: 1, Sequence: 2, ResultPaths: []string{"raw/sources/ch1.md"}},
		},
	}
	validated := validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" {
		t.Fatalf("shared raw evidence should not require a candidate page: %+v", validated)
	}

	ledger.Evidence["wiki/entities/tang.md"] = knowledgecore.KnowledgeCitation{Path: "wiki/entities/tang.md", Title: "唐僧", Kind: "entity"}
	ledger.EvidenceSequence["wiki/entities/tang.md"] = 4
	ledger.EvidenceSequence["raw/sources/ch1.md"] = 4
	validated = validateKnowledgeSubmission(base, ledger)
	if validated.Status != "complete" {
		t.Fatalf("read candidate page was not accepted: %+v", validated)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasValidationIssue(issues []knowledgecore.KnowledgeValidationIssue, code, requirementID string) bool {
	return validationIssue(issues, code, requirementID) != nil
}

func validationIssue(issues []knowledgecore.KnowledgeValidationIssue, code, requirementID string) *knowledgecore.KnowledgeValidationIssue {
	for _, issue := range issues {
		if issue.Code == code && issue.RequirementID == requirementID {
			return &issue
		}
	}
	return nil
}

func TestKnowledgeNineRequirementsReuseEvidenceAndRejectForgedPaths(t *testing.T) {
	base := knowledgecore.KnowledgeSubmission{Question: "Which candidate satisfies nine conditions?", Answer: "Candidate", Candidate: "Candidate"}
	for i := 1; i <= 9; i++ {
		id := fmt.Sprint(i)
		base.Requirements = append(base.Requirements, knowledgecore.KnowledgeRequirement{ID: id, Text: "condition " + id})
		base.Checks = append(base.Checks, knowledgecore.KnowledgeEvidenceCheck{RequirementID: id, Status: "supported", EvidencePaths: []string{"raw/sources/evidence.txt"}})
	}
	ledger := knowledgeLedger{Evidence: map[string]knowledgecore.KnowledgeCitation{"raw/sources/evidence.txt": {Path: "raw/sources/evidence.txt", Kind: "raw-source"}}}
	if got := validateKnowledgeSubmission(base, ledger); got.Status != "complete" {
		t.Fatalf("shared current-turn evidence rejected: %+v", got)
	}
	base.Checks[8].EvidencePaths = append(base.Checks[8].EvidencePaths, "wiki/invented.md")
	if got := validateKnowledgeSubmission(base, ledger); got.Status != "incomplete" || !hasValidationIssue(got.ValidationIssues, "evidence_not_in_current_turn", "9") {
		t.Fatalf("mixed real and invented citations accepted: %+v", got)
	}
	if got := validateKnowledgeSubmission(base, knowledgeLedger{}); got.Status != "incomplete" || len(got.UnresolvedRequirementIDs) != 9 {
		t.Fatalf("previous-turn evidence accepted: %+v", got)
	}
}
