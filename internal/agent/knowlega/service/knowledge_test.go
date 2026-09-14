package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

func TestKnowledgeSearchTermsNormalizeChinesePunctuationAndNGrams(t *testing.T) {
	terms := KnowledgeSearchTerms("“孙悟空，花果山”、大闹天宫。\n师父")
	for _, want := range []string{"孙悟空", "花果山", "大闹天宫", "师父", "孙悟", "悟空", "大闹", "闹天", "天宫", "大闹天", "闹天宫"} {
		if !containsKnowledgeString(terms, want) {
			t.Fatalf("terms=%v missing %q", terms, want)
		}
	}
	for _, term := range terms {
		if strings.ContainsAny(term, "，、。\n“”") {
			t.Fatalf("punctuation leaked into normalized term %q: %v", term, terms)
		}
	}
}

func TestKnowledgeSearchChinesePunctuationFindsRawAndWiki(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/monkey.md", "---\ntitle: 石猴\ntype: entity\naliases: [美猴王]\nsources: [raw/sources/chapter.md]\n---\n\n# 石猴\n\n孙悟空住在花果山。\n")
	writeKnowledgeFixture(t, root, "raw/sources/chapter.md", "# 第一回\n\n孙悟空住在花果山。\n")
	results, err := SearchProjectDocuments(t.Context(), root, "", "孙悟空，花果山。", 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsKnowledgeSearchPath(results, "wiki/entities/monkey.md") || !containsKnowledgeSearchPath(results, "raw/sources/chapter.md") {
		t.Fatalf("results=%+v", results)
	}
}

func TestKnowledgeSearchBoostsAliasesAndDownranksAggregates(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/auth.md", "---\ntitle: AuthService\ntype: entity\naliases: [Token Guard]\n---\n\n# AuthService\n\nValidates sessions.\n")
	writeKnowledgeFixture(t, root, "wiki/overview.md", "# Overview\n\nToken Guard Token Guard Token Guard\n")
	results, err := SearchProjectDocuments(t.Context(), root, "", "Token Guard", 3, nil, nil)
	if err != nil || len(results) == 0 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if results[0].Path != "wiki/entities/auth.md" {
		t.Fatalf("aggregate or body frequency won: %+v", results)
	}
}

func TestKnowledgeSearchTreatsOpaqueIdentifierQueriesAsExact(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/director.md", "---\ntitle: Operations Director\ntype: entity\n---\n\n# Operations Director\n\nMira Sol is the Operations Director.\n")
	writeKnowledgeFixture(t, root, "raw/sources/garden.md", "# Garden\n\nThe current ratio is CN-26.\n")
	negative, err := SearchProjectDocuments(t.Context(), root, "", "zqx-no-finance-director-91", 20, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(negative) != 0 {
		t.Fatalf("opaque negative query returned partial-term matches: %+v", negative)
	}
	positive, err := SearchProjectDocuments(t.Context(), root, "", "CN-26", 20, nil, nil)
	if err != nil || !containsKnowledgeSearchPath(positive, "raw/sources/garden.md") {
		t.Fatalf("exact identifier query did not find source: results=%+v err=%v", positive, err)
	}
}

func TestKnowledgeSearchUsesVectorAndFTSResultsWhenConfigured(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/file.md", "---\ntitle: File result\ntype: entity\n---\n\n# File result\n")
	store := &knowledgeSearchFake{
		vector: []core.KnowledgeSearchResult{{Path: "wiki/entities/vector.md", Title: "Vector result", Kind: "entity"}},
		fts:    []core.KnowledgeSearchResult{{Path: "wiki/entities/fts.md", Title: "FTS result", Kind: "entity"}},
	}
	embedder := &fakeKnowledgeEmbedding{value: []float32{0.1, 0.2}}
	results, err := SearchProjectDocuments(t.Context(), root, "project-1", "semantic query", 5, store, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 1 || store.vectorCalls != 1 || store.ftsCalls != 1 {
		t.Fatalf("backend order calls: embed=%d vector=%d fts=%d", embedder.calls, store.vectorCalls, store.ftsCalls)
	}
	if !containsKnowledgeSearchPath(results, "wiki/entities/vector.md") || !containsKnowledgeSearchPath(results, "wiki/entities/fts.md") {
		t.Fatalf("fused results=%+v", results)
	}
}

func TestKnowledgeSearchFallsBackToFilesWhenDatabaseBackendsFail(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/file.md", "---\ntitle: File result\ntype: entity\n---\n\n# File result\nsemantic query evidence\n")
	store := &knowledgeSearchFake{vectorErr: context.Canceled, ftsErr: context.Canceled}
	results, err := SearchProjectDocuments(t.Context(), root, "project-1", "semantic query", 5, store, &fakeKnowledgeEmbedding{value: []float32{0.1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Path != "wiki/entities/file.md" {
		t.Fatalf("file fallback results=%+v", results)
	}
}

func TestKnowledgeSearchAppendsWikiGraphExpansionAfterDirectRecall(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\n---\n\n# Alpha\n\nneedle evidence and [[Beta]].\n")
	writeKnowledgeFixture(t, root, "wiki/entities/beta.md", "---\ntitle: Beta\ntype: entity\n---\n\n# Beta\n\nRelated through the graph only.\n")
	results, err := SearchProjectDocuments(t.Context(), root, "", "needle", 5, nil, nil)
	if err != nil || !containsKnowledgeSearchPath(results, "wiki/entities/alpha.md") || !containsKnowledgeSearchPath(results, "wiki/entities/beta.md") {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if results[0].Path != "wiki/entities/alpha.md" {
		t.Fatalf("graph expansion displaced direct evidence: %+v", results)
	}
}

func TestKnowledgeReadResolvesAliasAndFollowLinksReadsEvidence(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/a.md", "---\ntitle: Alpha\ntype: entity\naliases: [A]\n---\n\n# Alpha\n\nSee [[Beta]].\n")
	writeKnowledgeFixture(t, root, "wiki/entities/b.md", "---\ntitle: Beta\ntype: entity\n---\n\n# Beta\n\nEvidence.\n")
	doc, err := ReadProjectDocument(root, "A")
	if err != nil || doc.Path != "wiki/entities/a.md" || !containsKnowledgeString(doc.Aliases, "A") {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	followed, err := FollowProjectLinks(root, doc.Path, 5)
	if err != nil || len(followed.Documents) != 1 || followed.Documents[0].Path != "wiki/entities/b.md" {
		t.Fatalf("followed=%+v err=%v", followed, err)
	}
}

func TestKnowledgeReadAndFollowResolveEverySupportedReferenceShape(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha Entity\ntype: entity\naliases: [A Prime]\n---\n\n# Alpha Entity\n\nSee [Beta](../concepts/beta.md).\n")
	writeKnowledgeFixture(t, root, "wiki/concepts/beta.md", "---\ntitle: Beta Concept\ntype: concept\n---\n\n# Beta Concept\n\nEvidence.\n")
	writeKnowledgeFixture(t, root, "raw/sources/alpha.md", "# Alpha Raw\n\nRaw evidence.\n")
	for _, target := range []string{"wiki/entities/alpha.md", "Alpha Entity", "A Prime", "[[Alpha Entity|display]]"} {
		doc, err := ReadProjectDocument(root, target)
		if err != nil || doc.Path != "wiki/entities/alpha.md" {
			t.Fatalf("target=%q doc=%+v err=%v", target, doc, err)
		}
	}
	raw, err := ReadProjectDocument(root, "raw/sources/alpha.md")
	if err != nil || raw.Path != "raw/sources/alpha.md" || raw.Kind != "raw-source" {
		t.Fatalf("raw=%+v err=%v", raw, err)
	}
	followed, err := FollowProjectLinks(root, "wiki/entities/alpha.md", 5)
	if err != nil || len(followed.Documents) != 1 || followed.Documents[0].Path != "wiki/concepts/beta.md" {
		t.Fatalf("relative follow=%+v err=%v", followed, err)
	}
}

func TestKnowledgeReadResolvesMovedPathBySameDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/current.md", "---\ntitle: Current\ntype: entity\naliases: [old-name]\n---\n\nPreserved evidence.\n")
	for _, target := range []string{"wiki/entities/old-name.md", "[[wiki/entities/old-name.md#Section|Old name]]"} {
		doc, err := ReadProjectDocument(root, target)
		if err != nil || doc.Path != "wiki/entities/current.md" {
			t.Fatalf("target=%q path=%q err=%v", target, doc.Path, err)
		}
	}
	for _, target := range []string{"wiki/concepts/old-name.md", "raw/sources/old-name.md", "wiki/../../old-name.md"} {
		if _, err := ReadProjectDocument(root, target); err == nil {
			t.Fatalf("unexpectedly resolved %q", target)
		}
	}
	writeKnowledgeFixture(t, root, "wiki/entities/another.md", "---\ntitle: Another\ntype: entity\naliases: [old-name]\n---\n\nOther evidence.\n")
	if _, err := ReadProjectDocument(root, "wiki/entities/old-name.md"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous alias error, got %v", err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/old-name.md", "---\ntitle: Exact\ntype: entity\n---\n\nExact evidence.\n")
	doc, err := ReadProjectDocument(root, "wiki/entities/old-name.md")
	if err != nil || doc.Path != "wiki/entities/old-name.md" {
		t.Fatalf("exact file must win: path=%q err=%v", doc.Path, err)
	}
}

func TestKnowledgeFollowLinksAcceptsRawSourcePath(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "raw/sources/a.md", "# Raw\n\nSee [[Alpha]].\n")
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\n---\n\n# Alpha\n\nEvidence.\n")
	result, err := FollowProjectLinks(root, "raw/sources/a.md", 5)
	if err != nil || len(result.Documents) != 1 || result.Documents[0].Path != "wiki/entities/alpha.md" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestKnowledgeDiscoverRanksDistinctRequirementCoverage(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\n---\n\n# Alpha\n\nred blue\n")
	writeKnowledgeFixture(t, root, "wiki/entities/beta.md", "---\ntitle: Beta\ntype: entity\n---\n\n# Beta\n\nred\n")
	candidates, err := DiscoverProjectCandidates(context.Background(), root, "", []core.KnowledgeRequirement{{ID: "r", Text: "red"}, {ID: "b", Text: "blue"}}, 5, nil, nil)
	if err != nil || len(candidates) == 0 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if candidates[0].Title != "Alpha" || candidates[0].RecallRequirementCount != 2 {
		t.Fatalf("candidates=%+v", candidates)
	}
}

func TestKnowledgeDiscoverAttributesManifestSummaryAndSharedSources(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "raw/sources/green.md", "# Green source\n\ngreen requirement\n")
	writeKnowledgeFixture(t, root, "wiki/entities/gamma.md", "---\ntitle: Gamma\ntype: entity\n---\n\n# Gamma\n\nCandidate page.\n")
	writeKnowledgeFixture(t, root, "wiki/entities/beta.md", "---\ntitle: Beta\ntype: entity\n---\n\n# Beta\n\nCandidate page.\n")
	writeKnowledgeFixture(t, root, "wiki/sources/red.md", "---\ntitle: Red source\ntype: source-summary\nsources: [raw/sources/red.md]\n---\n\n# Red source\n\nred requirement concerns [[Beta]].\n")
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\nsources: [raw/sources/shared.md]\n---\n\n# Alpha\n\nCandidate page.\n")
	writeKnowledgeFixture(t, root, "wiki/concepts/shared.md", "---\ntitle: Shared concept\ntype: concept\nsources: [raw/sources/shared.md]\n---\n\n# Shared concept\n\nblue requirement\n")
	if err := manifestfile.Save(root, manifestfile.File{Sources: map[string]manifestfile.Entry{
		"green.md": {OriginalPath: "green.md", RawPath: "raw/sources/green.md", Files: []string{"wiki/entities/gamma.md"}},
	}}); err != nil {
		t.Fatal(err)
	}
	candidates, err := DiscoverProjectCandidates(t.Context(), root, "", []core.KnowledgeRequirement{
		{ID: "green", Text: "green requirement"},
		{ID: "red", Text: "red requirement"},
		{ID: "blue", Text: "blue requirement"},
	}, 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"wiki/entities/gamma.md", "wiki/entities/beta.md", "wiki/entities/alpha.md"} {
		if !containsKnowledgeCandidatePath(candidates, path) {
			t.Fatalf("candidates=%+v missing %s", candidates, path)
		}
	}
}

func TestKnowledgeDiscoverDoesNotCountSharedSourceEntitiesAsDirectCoverage(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "raw/sources/shared.md", "# Shared\n\nmet the oracle\n")
	writeKnowledgeFixture(t, root, "wiki/entities/target.md", "---\ntitle: Target\ntype: entity\nsources: [raw/sources/shared.md]\n---\n\n# Target\n")
	writeKnowledgeFixture(t, root, "wiki/entities/decoy.md", "---\ntitle: Decoy\ntype: entity\nsources: [raw/sources/shared.md]\n---\n\n# Decoy\n")
	writeKnowledgeFixture(t, root, "wiki/sources/shared.md", "---\ntitle: Shared summary\ntype: source-summary\nsources: [raw/sources/shared.md]\n---\n\n# Shared summary\n\n[[Target]] met the oracle.\n")
	candidates, err := DiscoverProjectCandidates(t.Context(), root, "", []core.KnowledgeRequirement{{ID: "oracle", Text: "met the oracle"}}, 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	coverage := map[string]int{}
	for _, candidate := range candidates {
		coverage[candidate.Path] = candidate.RecallRequirementCount
	}
	if coverage["wiki/entities/target.md"] != 1 || coverage["wiki/entities/decoy.md"] != 0 {
		t.Fatalf("shared-source decoy received direct coverage: %+v", candidates)
	}
}

func TestKnowledgeDiscoverRanksLocalCommonSubjectAboveBroadRecall(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "raw/sources/scene.md", "# Scene\n\nTarget met Alpha and later met Beta.\n")
	writeKnowledgeFixture(t, root, "raw/sources/noise.md", "# Noise\n\nAlpha and Beta are listed here without the target event.\n")
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\naliases: [Alpha]\nsources: [raw/sources/scene.md]\n---\n\n# Alpha\n")
	writeKnowledgeFixture(t, root, "wiki/entities/beta.md", "---\ntitle: Beta\ntype: entity\naliases: [Beta]\nsources: [raw/sources/scene.md]\n---\n\n# Beta\n")
	writeKnowledgeFixture(t, root, "wiki/entities/target.md", "---\ntitle: Target\ntype: entity\naliases: [Target]\nsources: [raw/sources/scene.md]\n---\n\n# Target\n\nTarget met Alpha and later met Beta.\n")
	writeKnowledgeFixture(t, root, "wiki/entities/noisy.md", "---\ntitle: Noisy\ntype: entity\naliases: [Noisy]\nsources: [raw/sources/noise.md]\n---\n\n# Noisy\n\nAlpha Beta met met met.\n")

	candidates, err := DiscoverProjectCandidates(t.Context(), root, "", []core.KnowledgeRequirement{
		{ID: "alpha", Text: "met Alpha"},
		{ID: "beta", Text: "met Beta"},
	}, 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 || candidates[0].Path != "wiki/entities/target.md" || candidates[0].RecallRequirementCount != 2 {
		t.Fatalf("local common subject did not rank first: %+v", candidates)
	}
}

func TestKnowledgeGraphResolvesAliasAndRawSourceSeeds(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "raw/sources/shared.md", "# Shared source\n")
	writeKnowledgeFixture(t, root, "wiki/entities/alpha.md", "---\ntitle: Alpha\ntype: entity\naliases: [A Prime]\nsources: [raw/sources/shared.md]\n---\n\n# Alpha\n\n[[Beta]]\n")
	writeKnowledgeFixture(t, root, "wiki/entities/beta.md", "---\ntitle: Beta\ntype: entity\nsources: [raw/sources/shared.md]\n---\n\n# Beta\n\nRelated evidence.\n")
	for _, seed := range []string{"A Prime", "raw/sources/shared.md"} {
		docs, err := SearchProjectGraph(t.Context(), root, "", "", []string{seed}, 5, nil)
		if err != nil || !containsKnowledgeDocumentPath(docs, "wiki/entities/beta.md") {
			t.Fatalf("seed=%q docs=%+v err=%v", seed, docs, err)
		}
	}
}

func TestKnowledgeWritebackIsExplicitEvidenceBackedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/a.md", "---\ntitle: Alpha\ntype: entity\n---\n\n# Alpha\n\nEvidence.\n")
	submission := core.KnowledgeSubmission{Question: "Who?", Answer: "Alpha", Status: "complete", Citations: []core.KnowledgeCitation{{Path: "wiki/entities/a.md", Title: "Alpha", Kind: "entity"}}}
	first, err := WriteKnowledgeSubmission(KnowledgeWritebackOptions{ProjectPath: root, Title: "Alpha answer", Submission: submission})
	if err != nil || !first.Written {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	synthesisPath := filepath.Join(root, filepath.FromSlash(first.Path))
	firstContent, err := os.ReadFile(synthesisPath)
	if err != nil {
		t.Fatal(err)
	}
	oldContent := strings.Replace(string(firstContent), `created: "`+time.Now().Format("2006-01-02")+`"`, `created: "2001-02-03"`, 1)
	if oldContent == string(firstContent) {
		t.Fatalf("synthesis missing created date:\n%s", firstContent)
	}
	if err := os.WriteFile(synthesisPath, []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := WriteKnowledgeSubmission(KnowledgeWritebackOptions{ProjectPath: root, Title: "Alpha answer", Submission: submission})
	if err != nil || second.Written {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	after, err := os.ReadFile(synthesisPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != oldContent {
		t.Fatalf("idempotent write changed the original created date:\n%s", after)
	}
	logData, _ := os.ReadFile(filepath.Join(root, "wiki", "log.md"))
	if strings.Count(string(logData), "knowledge-writeback:") != 1 {
		t.Fatalf("log=%s", logData)
	}
}

func writeKnowledgeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type knowledgeSearchFake struct {
	vector, fts           []core.KnowledgeSearchResult
	vectorErr, ftsErr     error
	vectorCalls, ftsCalls int
}

func (s *knowledgeSearchFake) SearchWikiEvidenceVector(_ context.Context, _ string, _ KnowledgeSearchPlan, _ []float32) ([]core.KnowledgeSearchResult, error) {
	s.vectorCalls++
	return s.vector, s.vectorErr
}

func (s *knowledgeSearchFake) SearchWikiEvidence(_ context.Context, _ string, _ KnowledgeSearchPlan) ([]core.KnowledgeSearchResult, error) {
	s.ftsCalls++
	return s.fts, s.ftsErr
}

type fakeKnowledgeEmbedding struct {
	value []float32
	calls int
}

func (p *fakeKnowledgeEmbedding) EmbedText(context.Context, string) ([]float32, error) {
	p.calls++
	return p.value, nil
}

func containsKnowledgeSearchPath(results []core.KnowledgeSearchResult, path string) bool {
	for _, result := range results {
		if result.Path == path {
			return true
		}
	}
	return false
}

func containsKnowledgeString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsKnowledgeCandidatePath(results []core.KnowledgeCandidate, path string) bool {
	for _, result := range results {
		if result.Path == path {
			return true
		}
	}
	return false
}

func containsKnowledgeDocumentPath(results []KnowledgeDocument, path string) bool {
	for _, result := range results {
		if result.Path == path {
			return true
		}
	}
	return false
}

func TestScopedKnowledgeSearchSelectsCorpusBeforeRanking(t *testing.T) {
	root := t.TempDir()
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "test"}); err != nil {
		t.Fatal(err)
	}
	writeKnowledgeFixture(t, root, "wiki/entities/ruler.md", "---\ntitle: Ruler\ntype: entity\n---\n\nThe ruler offers wine.")
	writeKnowledgeFixture(t, root, "raw/sources/chapter.txt", "The ruler insists on wine and the traveller accepts.")
	for _, scope := range []string{"raw", "wiki"} {
		results, err := SearchProjectDocumentsScoped(t.Context(), root, "", "wine", 1, scope, nil, nil)
		if err != nil || len(results) != 1 || !strings.HasPrefix(results[0].Path, scope+"/") {
			t.Fatalf("scope=%s results=%+v err=%v", scope, results, err)
		}
	}
	if _, err := SearchProjectDocumentsScoped(t.Context(), root, "", "wine", 1, "invalid", nil, nil); err == nil {
		t.Fatal("invalid scope accepted")
	}
}
