package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

var codeIndexPersistMu sync.Mutex

type GoCodeIndexOptions struct {
	ProjectPath  string
	RepoID       string
	RepoPath     string
	Commit       string
	IncludeTests bool
}

type GoCodeIndexResult struct {
	RepoID       string             `json:"repo_id"`
	Commit       string             `json:"commit"`
	Dirty        bool               `json:"dirty"`
	SourceSHA256 string             `json:"source_sha256"`
	SnapshotPath string             `json:"snapshot_path"`
	ReportPath   string             `json:"report_path"`
	OverviewPath string             `json:"overview_path"`
	WrittenPaths []string           `json:"written_paths"`
	Nodes        int                `json:"nodes"`
	Edges        int                `json:"edges"`
	Communities  int                `json:"communities"`
	Processes    int                `json:"processes"`
	Snapshot     codegraph.Snapshot `json:"-"`
}

type codeSnapshotPointer struct {
	Version      int    `json:"version"`
	RepoID       string `json:"repo_id"`
	Commit       string `json:"commit"`
	Dirty        bool   `json:"dirty,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	SnapshotPath string `json:"snapshot_path"`
	UpdatedAt    string `json:"updated_at"`
}

func IndexGoCodeSnapshot(opts GoCodeIndexOptions) (GoCodeIndexResult, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return GoCodeIndexResult{}, fmt.Errorf("project path is required")
	}
	release, err := acquireServiceProjectLock(opts.ProjectPath)
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	defer release()
	snapshot, err := codegraph.IndexGoRepository(codegraph.GoIndexOptions{
		RepoID: opts.RepoID, RepoPath: opts.RepoPath, Commit: opts.Commit, IncludeTests: opts.IncludeTests,
	})
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	if previous, ok := loadLatestCodeSnapshot(opts.ProjectPath, snapshot.RepoID); ok {
		codegraph.ReconcileCommunityIDs(&previous, &snapshot)
		snapshot.ReportBody = codegraph.RenderGraphReport(snapshot)
	}
	codeIndexPersistMu.Lock()
	defer codeIndexPersistMu.Unlock()
	commitDir := codeSnapshotRevisionDir(snapshot)
	snapshotDirRel := filepath.ToSlash(filepath.Join(".kbcore", "graph-snapshots", "code", snapshot.RepoID, commitDir))
	snapshotPath := filepath.ToSlash(filepath.Join(snapshotDirRel, "graph.json"))
	reportPath := filepath.ToSlash(filepath.Join(snapshotDirRel, "GRAPH_REPORT.md"))
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(opts.ProjectPath, filepath.FromSlash(snapshotDirRel)), 0o755); err != nil {
		return GoCodeIndexResult{}, err
	}
	if err := writeFileAtomic(filepath.Join(opts.ProjectPath, filepath.FromSlash(snapshotPath)), data); err != nil {
		return GoCodeIndexResult{}, err
	}
	if err := writeFileAtomic(filepath.Join(opts.ProjectPath, filepath.FromSlash(reportPath)), []byte(snapshot.ReportBody)); err != nil {
		return GoCodeIndexResult{}, err
	}
	pointer := codeSnapshotPointer{
		Version: 1, RepoID: snapshot.RepoID, Commit: snapshot.Commit, Dirty: snapshot.Dirty,
		SourceSHA256: snapshot.SourceSHA256, SnapshotPath: snapshotPath, UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	pointerData, err := json.MarshalIndent(pointer, "", "  ")
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	pointerData = append(pointerData, '\n')
	if err := os.MkdirAll(filepath.Join(opts.ProjectPath, ".kbcore", "graph-snapshots", "code", snapshot.RepoID), 0o755); err != nil {
		return GoCodeIndexResult{}, err
	}
	if err := writeFileAtomic(filepath.Join(opts.ProjectPath, ".kbcore", "graph-snapshots", "code", snapshot.RepoID, "latest.json"), pointerData); err != nil {
		return GoCodeIndexResult{}, err
	}

	overviewPath := filepath.ToSlash(filepath.Join("wiki", "code", snapshot.RepoID, "overview.md"))
	written := []string{overviewPath}
	publishedCommunities := publishableCodeCommunities(snapshot.Communities, 60)
	if err := wiki.WriteVersionedPage(opts.ProjectPath, overviewPath, []byte(renderNativeCodeOverview(snapshot, snapshotPath)), "code-index: "+snapshot.RepoID); err != nil {
		return GoCodeIndexResult{}, err
	}
	keepGenerated := map[string]bool{}
	for _, community := range publishedCommunities {
		rel := filepath.ToSlash(filepath.Join("wiki", "code", snapshot.RepoID, "communities", community.ID+".md"))
		if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(renderCodeCommunityPage(snapshot, community, snapshotPath)), "code-index community: "+snapshot.RepoID); err != nil {
			return GoCodeIndexResult{}, err
		}
		written = append(written, rel)
		keepGenerated[rel] = true
	}
	for _, process := range snapshot.Processes {
		rel := filepath.ToSlash(filepath.Join("wiki", "code", snapshot.RepoID, "processes", process.ID+".md"))
		if err := wiki.WriteVersionedPage(opts.ProjectPath, rel, []byte(renderCodeProcessPage(snapshot, process, snapshotPath)), "code-index process: "+snapshot.RepoID); err != nil {
			return GoCodeIndexResult{}, err
		}
		written = append(written, rel)
		keepGenerated[rel] = true
	}
	if err := pruneStaleCodeGeneratedPages(opts.ProjectPath, snapshot.RepoID, keepGenerated); err != nil {
		return GoCodeIndexResult{}, err
	}
	for _, rel := range written {
		if err := registerPageOwnership(opts.ProjectPath, rel, "code"); err != nil {
			return GoCodeIndexResult{}, err
		}
	}
	_ = appendIndex(opts.ProjectPath, "Code", snapshot.RepoID+" Code Overview", overviewPath)
	_ = appendLog(opts.ProjectPath, "code-index", snapshot.RepoID, fmt.Sprintf("Indexed Go repository commit `%s` with %d nodes, %d edges, %d communities, and %d processes.", snapshot.Commit, len(snapshot.Nodes), len(snapshot.Edges), len(snapshot.Communities), len(snapshot.Processes)))
	if err := RefreshRelationsArtifact(opts.ProjectPath); err != nil {
		return GoCodeIndexResult{}, fmt.Errorf("refresh generated relations: %w", err)
	}
	return GoCodeIndexResult{
		RepoID: snapshot.RepoID, Commit: snapshot.Commit, Dirty: snapshot.Dirty, SourceSHA256: snapshot.SourceSHA256,
		SnapshotPath: snapshotPath, ReportPath: reportPath,
		OverviewPath: overviewPath, WrittenPaths: written, Nodes: len(snapshot.Nodes), Edges: len(snapshot.Edges),
		Communities: len(snapshot.Communities), Processes: len(snapshot.Processes), Snapshot: snapshot,
	}, nil
}

func codeSnapshotRevisionDir(snapshot codegraph.Snapshot) string {
	revision := strings.TrimSpace(snapshot.Commit)
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		revision = core.Slug(revision)
	}
	hash := snapshot.SourceSHA256
	if len(hash) > 12 {
		hash = hash[:12]
	}
	if revision == "" {
		revision = "working-tree"
		if hash != "" {
			revision += "-" + hash
		}
	} else if snapshot.Dirty {
		revision += "-dirty"
		if hash != "" {
			revision += "-" + hash
		}
	}
	return revision
}

func loadLatestCodeSnapshot(projectPath, repoID string) (codegraph.Snapshot, bool) {
	pointer, ok := loadLatestCodeSnapshotPointer(projectPath, repoID)
	if !ok {
		return codegraph.Snapshot{}, false
	}
	data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(pointer.SnapshotPath)))
	if err != nil {
		return codegraph.Snapshot{}, false
	}
	var snapshot codegraph.Snapshot
	if json.Unmarshal(data, &snapshot) != nil {
		return codegraph.Snapshot{}, false
	}
	return snapshot, true
}

func loadLatestCodeSnapshotPointer(projectPath, repoID string) (codeSnapshotPointer, bool) {
	pointerData, err := os.ReadFile(filepath.Join(projectPath, ".kbcore", "graph-snapshots", "code", repoID, "latest.json"))
	if err != nil {
		return codeSnapshotPointer{}, false
	}
	var pointer codeSnapshotPointer
	if json.Unmarshal(pointerData, &pointer) != nil || pointer.SnapshotPath == "" {
		return codeSnapshotPointer{}, false
	}
	if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(pointer.SnapshotPath))); err != nil {
		return codeSnapshotPointer{}, false
	}
	return pointer, true
}

func renderNativeCodeOverview(snapshot codegraph.Snapshot, sourcePath string) string {
	body := codegraph.RenderGraphReport(snapshot)
	page := wiki.Page{
		Title: snapshot.RepoID + " Code Overview", Type: "code-overview", Sources: []string{sourcePath}, GeneratedBy: "knowledge-core/go-native",
		Extra: codeSnapshotFrontmatter(snapshot, map[string]string{"code_repo": snapshot.RepoID, "graph_source": snapshot.Source}),
		Body:  body + "\n## Navigation\n\n" + renderCodeNavigation(snapshot, publishableCodeCommunities(snapshot.Communities, 60)),
	}
	return wiki.RenderPage(page)
}

func renderCodeCommunityPage(snapshot codegraph.Snapshot, community codegraph.Community, sourcePath string) string {
	nodes := snapshotNodeMap(snapshot)
	var members []string
	for _, id := range community.Members {
		if node, ok := nodes[id]; ok {
			members = append(members, fmt.Sprintf("- `%s` (%s, `%s`)", node.Label, node.Kind, id))
		}
	}
	sort.Strings(members)
	page := wiki.Page{
		Title: community.Label, Type: "code-community", Sources: []string{sourcePath}, GeneratedBy: "knowledge-core/go-native",
		Extra: codeSnapshotFrontmatter(snapshot, map[string]string{
			"code_repo": snapshot.RepoID, "community_id": community.ID,
			"cohesion": fmt.Sprintf("%.4f", community.Cohesion), "symbol_ids": strings.Join(community.Members, ","),
		}),
		Body: fmt.Sprintf("# %s\n\n## Community\n\n- Members: %d\n- Cohesion: %.4f\n\n## Symbols\n\n%s\n\nSee [[wiki/code/%s/overview.md|%s Code Overview]].\n", community.Label, len(community.Members), community.Cohesion, strings.Join(members, "\n"), snapshot.RepoID, snapshot.RepoID),
	}
	return wiki.RenderPage(page)
}

func renderCodeProcessPage(snapshot codegraph.Snapshot, process codegraph.Process, sourcePath string) string {
	nodes := snapshotNodeMap(snapshot)
	var steps []string
	for index, id := range process.Steps {
		node := nodes[id]
		steps = append(steps, fmt.Sprintf("%d. `%s` (%s, `%s`)", index+1, node.Label, node.Kind, id))
	}
	page := wiki.Page{
		Title: process.Label, Type: "code-process", Sources: []string{sourcePath}, GeneratedBy: "knowledge-core/go-native",
		Extra: codeSnapshotFrontmatter(snapshot, map[string]string{
			"code_repo": snapshot.RepoID, "process_id": process.ID,
			"entry_symbol": process.EntryID, "symbol_ids": strings.Join(process.Steps, ","),
		}),
		Body: fmt.Sprintf("# %s\n\n## Process Flow\n\n%s\n\nSee [[wiki/code/%s/overview.md|%s Code Overview]].\n", process.Label, strings.Join(steps, "\n"), snapshot.RepoID, snapshot.RepoID),
	}
	return wiki.RenderPage(page)
}

func codeSnapshotFrontmatter(snapshot codegraph.Snapshot, values map[string]string) map[string]string {
	values["commit"] = snapshot.Commit
	values["working_tree_dirty"] = fmt.Sprint(snapshot.Dirty)
	values["source_sha256"] = snapshot.SourceSHA256
	return values
}

func renderCodeNavigation(snapshot codegraph.Snapshot, communities []codegraph.Community) string {
	var lines []string
	for _, community := range communities {
		lines = append(lines, fmt.Sprintf("- Community: [[communities/%s|%s]]", community.ID, community.Label))
	}
	for _, process := range snapshot.Processes {
		lines = append(lines, fmt.Sprintf("- Process: [[processes/%s|%s]]", process.ID, process.Label))
	}
	if len(lines) == 0 {
		return "No multi-node communities or processes were detected.\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func publishableCodeCommunities(communities []codegraph.Community, limit int) []codegraph.Community {
	var candidates []codegraph.Community
	for _, community := range communities {
		if len(community.Members) >= 4 || (len(community.Members) >= 3 && community.Cohesion >= 0.55) {
			candidates = append(candidates, community)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].Members) != len(candidates[j].Members) {
			return len(candidates[i].Members) > len(candidates[j].Members)
		}
		if candidates[i].Cohesion != candidates[j].Cohesion {
			return candidates[i].Cohesion > candidates[j].Cohesion
		}
		return candidates[i].ID < candidates[j].ID
	})
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func pruneStaleCodeGeneratedPages(projectPath, repoID string, keep map[string]bool) error {
	for _, section := range []string{"communities", "processes"} {
		root := filepath.Join(projectPath, "wiki", "code", repoID, section)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		if err := filepath.WalkDir(root, func(filename string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				return walkErr
			}
			rel, err := filepath.Rel(projectPath, filename)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if keep[rel] {
				return nil
			}
			return deleteWikiPageWithArchive(projectPath, rel, "code-index prune: "+repoID)
		}); err != nil {
			return err
		}
	}
	return nil
}

func snapshotNodeMap(snapshot codegraph.Snapshot) map[string]codegraph.Node {
	out := make(map[string]codegraph.Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		out[node.ID] = node
	}
	return out
}

func NativeSnapshotSourceRef(result GoCodeIndexResult) string {
	return result.SnapshotPath
}

func NativeCodeRepoID(projectID, repoID string) string {
	return core.StableID(projectID, "repo", repoID)
}
