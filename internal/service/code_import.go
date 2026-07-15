package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type CodeImportOptions struct {
	ProjectPath string
	RepoID      string
	RepoPath    string
	GraphPath   string
	ReportPath  string
}

type CodeImportResult struct {
	SnapshotDir string
	Overview    string
	NodeCount   int
	EdgeCount   int
}

type CodeGraphStore = core.CodeGraphStore

type TransactionalCodeGraphStore interface {
	CodeGraphStore
	WithCodeGraphStoreTx(context.Context, func(CodeGraphStore) error) error
}

type CodeGraphSyncResult struct {
	RepoID string
	Nodes  int
	Edges  int
}

func ImportGraphifyCodeSnapshot(opts CodeImportOptions) (CodeImportResult, error) {
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return CodeImportResult{}, err
	}
	defer release()
	if opts.RepoID == "" {
		opts.RepoID = core.Slug(filepath.Base(opts.RepoPath))
	}
	snap, err := codegraph.ImportGraphify(opts.RepoID, opts.RepoPath, opts.GraphPath, opts.ReportPath)
	if err != nil {
		return CodeImportResult{}, err
	}
	snapshotRel := filepath.Join("raw", "code-graphs", opts.RepoID, "graphify")
	snapshotAbs := filepath.Join(opts.ProjectPath, snapshotRel)
	if err := os.MkdirAll(snapshotAbs, 0o755); err != nil {
		return CodeImportResult{}, err
	}
	if err := copyFile(opts.GraphPath, filepath.Join(snapshotAbs, "graph.json")); err != nil {
		return CodeImportResult{}, err
	}
	if opts.ReportPath != "" {
		_ = copyFile(opts.ReportPath, filepath.Join(snapshotAbs, "GRAPH_REPORT.md"))
	}

	overviewRel := filepath.Join("wiki", "code", opts.RepoID, "overview.md")
	page := wiki.RenderPage(wiki.Page{
		Title:       opts.RepoID + " Code Overview",
		Type:        "code-overview",
		Sources:     []string{filepath.ToSlash(filepath.Join(snapshotRel, "graph.json"))},
		GeneratedBy: "knowledge-core",
		Extra: map[string]string{
			"code_repo":     opts.RepoID,
			"graph_sources": "graphify",
		},
		Body: renderCodeOverview(snap),
	})
	if err := wiki.WriteVersionedPage(opts.ProjectPath, filepath.ToSlash(overviewRel), []byte(page), "code-import: "+opts.RepoID); err != nil {
		return CodeImportResult{}, err
	}
	if err := registerPageOwnership(opts.ProjectPath, filepath.ToSlash(overviewRel), "code"); err != nil {
		return CodeImportResult{}, err
	}
	_ = appendLog(opts.ProjectPath, "code-import", opts.RepoID, fmt.Sprintf("Imported graphify snapshot with %d nodes and %d edges.", len(snap.Nodes), len(snap.Edges)))
	_ = appendIndex(opts.ProjectPath, "Code", opts.RepoID+" Code Overview", overviewRel)
	return CodeImportResult{
		SnapshotDir: filepath.ToSlash(snapshotRel),
		Overview:    filepath.ToSlash(overviewRel),
		NodeCount:   len(snap.Nodes),
		EdgeCount:   len(snap.Edges),
	}, nil
}

func SyncCodeGraphSnapshot(ctx context.Context, store CodeGraphStore, projectID string, snap codegraph.Snapshot, sourceRef string) (CodeGraphSyncResult, error) {
	if txStore, ok := store.(TransactionalCodeGraphStore); ok {
		var result CodeGraphSyncResult
		err := txStore.WithCodeGraphStoreTx(ctx, func(store CodeGraphStore) error {
			var syncErr error
			result, syncErr = syncCodeGraphSnapshot(ctx, store, projectID, snap, sourceRef)
			return syncErr
		})
		return result, err
	}
	return syncCodeGraphSnapshot(ctx, store, projectID, snap, sourceRef)
}

func syncCodeGraphSnapshot(ctx context.Context, store CodeGraphStore, projectID string, snap codegraph.Snapshot, sourceRef string) (CodeGraphSyncResult, error) {
	if store == nil {
		return CodeGraphSyncResult{}, fmt.Errorf("code graph store is required")
	}
	if projectID == "" {
		return CodeGraphSyncResult{}, fmt.Errorf("project id is required")
	}
	if snap.RepoID == "" {
		return CodeGraphSyncResult{}, fmt.Errorf("repo id is required")
	}
	now := snap.ImportedAt
	if now.IsZero() {
		now = timeNow()
	}
	graphSource := snap.Source
	if graphSource == "" {
		graphSource = "graphify"
	}
	repoID := graphRepoID(projectID, snap.RepoID)
	if err := store.UpsertCodeRepo(ctx, core.CodeRepo{
		ID:                 repoID,
		ProjectID:          projectID,
		RepoPath:           snap.RepoPath,
		HeadCommit:         snap.Commit,
		IndexedCommit:      indexedCodeRevision(snap),
		PrimaryGraphSource: graphSource,
		Stale:              false,
		SyncedAt:           &now,
	}); err != nil {
		return CodeGraphSyncResult{}, err
	}
	if err := store.DeleteGraphFacts(ctx, projectID, repoID); err != nil {
		return CodeGraphSyncResult{}, err
	}
	nodeIDs := make(map[string]string, len(snap.Nodes))
	for _, node := range snap.Nodes {
		nodeID := graphNodeID(projectID, snap.RepoID, node.ID)
		nodeIDs[node.ID] = nodeID
		props := copyMap(node.Props)
		props["raw_id"] = node.ID
		props["community"] = node.Community
		props["graph_source"] = graphSource
		props["commit"] = snap.Commit
		props["working_tree_dirty"] = snap.Dirty
		props["source_sha256"] = snap.SourceSHA256
		if err := store.AddGraphNode(ctx, core.GraphNode{
			ID:        nodeID,
			ProjectID: projectID,
			RepoID:    repoID,
			Domain:    "code",
			ScopeID:   snap.RepoID,
			Kind:      string(node.Kind),
			Label:     node.Label,
			SourceRef: graphSourceRef(sourceRef, "node", node.ID),
			Props:     props,
		}); err != nil {
			return CodeGraphSyncResult{}, err
		}
	}
	for _, edge := range snap.Edges {
		srcID := nodeIDs[edge.Source]
		if srcID == "" {
			srcID = graphNodeID(projectID, snap.RepoID, edge.Source)
		}
		dstID := nodeIDs[edge.Target]
		if dstID == "" {
			dstID = graphNodeID(projectID, snap.RepoID, edge.Target)
		}
		props := copyMap(edge.Props)
		props["raw_source"] = edge.Source
		props["raw_target"] = edge.Target
		props["graph_source"] = graphSource
		props["commit"] = snap.Commit
		props["working_tree_dirty"] = snap.Dirty
		props["source_sha256"] = snap.SourceSHA256
		props["source_ref"] = graphSourceRef(sourceRef, "edge", edge.Source+"/"+edge.Target)
		if err := store.AddGraphEdge(ctx, core.GraphEdge{
			ID:              graphEdgeID(projectID, snap.RepoID, edge.Source, edge.Target, string(edge.Relation)),
			ProjectID:       projectID,
			RepoID:          repoID,
			Domain:          "code",
			ScopeID:         snap.RepoID,
			SourceID:        srcID,
			TargetID:        dstID,
			Relation:        string(edge.Relation),
			Confidence:      graphConfidence(edge.Confidence),
			ConfidenceScore: graphEdgeConfidenceScore(edge),
			Weight:          edge.Weight,
			Evidence:        append([]string(nil), edge.Evidence...),
			Props:           props,
		}); err != nil {
			return CodeGraphSyncResult{}, err
		}
	}
	return CodeGraphSyncResult{
		RepoID: repoID,
		Nodes:  len(snap.Nodes),
		Edges:  len(snap.Edges),
	}, nil
}

func indexedCodeRevision(snapshot codegraph.Snapshot) string {
	commit := strings.TrimSpace(snapshot.Commit)
	if !snapshot.Dirty {
		return commit
	}
	hash := snapshot.SourceSHA256
	if len(hash) > 12 {
		hash = hash[:12]
	}
	if commit == "" {
		if hash == "" {
			return "dirty"
		}
		return "dirty." + hash
	}
	if hash == "" {
		return commit + "+dirty"
	}
	return commit + "+dirty." + hash
}

func graphEdgeConfidenceScore(edge codegraph.Edge) float64 {
	if edge.ConfidenceScore > 0 {
		return edge.ConfidenceScore
	}
	switch graphConfidence(edge.Confidence) {
	case core.ConfidenceExtracted:
		return 1
	case core.ConfidenceAmbiguous:
		return 0.3
	default:
		return 0.75
	}
}

func renderCodeOverview(snap codegraph.Snapshot) string {
	kindCounts := map[codegraph.NodeKind]int{}
	relations := map[codegraph.Relation]int{}
	for _, node := range snap.Nodes {
		kindCounts[node.Kind]++
	}
	for _, edge := range snap.Edges {
		relations[edge.Relation]++
	}
	var kinds []string
	for kind, count := range kindCounts {
		kinds = append(kinds, fmt.Sprintf("- %s: %d", kind, count))
	}
	sort.Strings(kinds)
	var rels []string
	for rel, count := range relations {
		rels = append(rels, fmt.Sprintf("- %s: %d", rel, count))
	}
	sort.Strings(rels)

	return fmt.Sprintf(
		"# %s Code Overview\n\n"+
			"## Snapshot\n\n"+
			"- Source: `%s`\n"+
			"- Repo path: `%s`\n"+
			"- Nodes: %d\n"+
			"- Edges: %d\n\n"+
			"## Node Kinds\n\n"+
			"%s\n\n"+
			"## Relations\n\n"+
			"%s\n\n"+
			"## Notes\n\n"+
			"This page is compiled from a graphify snapshot. GitNexus-style semantic indexing should be treated as the primary source for exact calls, context, impact, and trace queries.\n",
		snap.RepoID, snap.Source, snap.RepoPath, len(snap.Nodes), len(snap.Edges), strings.Join(kinds, "\n"), strings.Join(rels, "\n"),
	)
}

func graphRepoID(projectID, repoID string) string {
	return core.StableID(projectID, "repo", repoID)
}

func graphNodeID(projectID, repoID, rawID string) string {
	return core.StableID(projectID, "repo", repoID, "node", rawID)
}

func graphEdgeID(projectID, repoID, source, target, relation string) string {
	return core.StableID(projectID, "repo", repoID, "edge", source, target, relation)
}

func graphSourceRef(sourceRef, kind, rawID string) string {
	if sourceRef == "" {
		return kind + "/" + rawID
	}
	return filepath.ToSlash(sourceRef) + "#" + kind + "/" + rawID
}

func graphConfidence(value string) core.Confidence {
	switch core.Confidence(strings.ToUpper(strings.TrimSpace(value))) {
	case core.ConfidenceExtracted:
		return core.ConfidenceExtracted
	case core.ConfidenceAmbiguous:
		return core.ConfidenceAmbiguous
	default:
		return core.ConfidenceInferred
	}
}

func copyMap(values map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

var timeNow = func() time.Time {
	return time.Now()
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data)
}
