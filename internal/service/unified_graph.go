package service

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type unifiedGraphCacheEntry struct {
	Signature uint64
	Graph     WikiGraphAPIResult
}

var unifiedGraphCache sync.Map
var relationsArtifactMu sync.Mutex

type ProjectGraphQuery struct {
	ProjectPath        string   `json:"project_path"`
	SeedIDs            []string `json:"seed_ids"`
	Query              string   `json:"query"`
	Domains            []string `json:"domains"`
	Kinds              []string `json:"kinds"`
	Relations          []string `json:"relations"`
	Confidence         []string `json:"confidence"`
	MinConfidenceScore float64  `json:"min_confidence_score"`
	Depth              int      `json:"depth"`
	Limit              int      `json:"limit"`
	Cursor             string   `json:"cursor"`
}

type relationsArtifact struct {
	Version     int                `json:"version"`
	GeneratedAt string             `json:"generated_at"`
	GeneratedBy string             `json:"generated_by"`
	Edges       []WikiGraphAPIEdge `json:"edges"`
}

func BuildUnifiedProjectGraph(projectPath string) (WikiGraphAPIResult, error) {
	signature := unifiedGraphInputSignature(projectPath)
	if cached, ok := unifiedGraphCache.Load(projectPath); ok {
		entry := cached.(unifiedGraphCacheEntry)
		if entry.Signature == signature {
			return entry.Graph, nil
		}
	}
	graph, err := buildUnifiedProjectGraph(projectPath)
	if err == nil {
		unifiedGraphCache.Store(projectPath, unifiedGraphCacheEntry{Signature: signature, Graph: graph})
	}
	return graph, err
}

func buildUnifiedProjectGraph(projectPath string) (WikiGraphAPIResult, error) {
	wikiGraph, err := BuildWikiGraph(projectPath)
	if err != nil {
		return WikiGraphAPIResult{}, err
	}
	nodes := map[string]WikiGraphAPINode{}
	edges := map[string]WikiGraphAPIEdge{}
	addNode := func(node WikiGraphAPINode) {
		if node.ID == "" {
			node.ID = firstNonEmptyString(node.Path, core.StableID(node.Domain, node.ScopeID, node.Label))
		}
		if node.Label == "" {
			node.Label = firstNonEmptyString(node.Title, node.Path, node.ID)
		}
		if node.Title == "" {
			node.Title = node.Label
		}
		if node.Kind == "" {
			node.Kind = node.Type
		}
		if node.Type == "" {
			node.Type = node.Kind
		}
		if node.Props == nil {
			node.Props = map[string]any{}
		}
		nodes[node.ID] = node
	}
	addEdge := func(edge WikiGraphAPIEdge) {
		if edge.Source == "" || edge.Target == "" || edge.Source == edge.Target {
			return
		}
		if edge.Relation == "" {
			edge.Relation = strings.ToUpper(edge.Kind)
		}
		if edge.Kind == "" {
			edge.Kind = strings.ToLower(edge.Relation)
		}
		if edge.Confidence == "" {
			edge.Confidence = "EXTRACTED"
		}
		if edge.ConfidenceScore == 0 && edge.Confidence == "EXTRACTED" {
			edge.ConfidenceScore = 1
		}
		if edge.Weight == 0 {
			edge.Weight = 1
		}
		if edge.ID == "" {
			edge.ID = core.StableID("project-graph", edge.Source, edge.Target, edge.Relation)
		}
		key := edge.Source + "\x00" + edge.Target + "\x00" + edge.Relation
		if previous, ok := edges[key]; ok {
			previous.Weight += edge.Weight
			previous.Evidence = appendUniqueStrings(previous.Evidence, edge.Evidence...)
			if edge.ConfidenceScore > previous.ConfidenceScore {
				previous.Confidence = edge.Confidence
				previous.ConfidenceScore = edge.ConfidenceScore
			}
			edges[key] = previous
			return
		}
		edges[key] = edge
	}

	wikiPaths := make([]string, 0, len(wikiGraph.Nodes))
	for path := range wikiGraph.Nodes {
		wikiPaths = append(wikiPaths, path)
	}
	sort.Strings(wikiPaths)
	for _, pagePath := range wikiPaths {
		page := wikiGraph.Nodes[pagePath]
		addNode(WikiGraphAPINode{
			ID: page.Path, Path: page.Path, Title: page.Title, Label: page.Title,
			Type: page.Type, Kind: page.Type, Domain: "wiki", Sources: append([]string(nil), page.Sources...),
			SourceRef: page.Path, Props: map[string]any{"body_chars": len(page.Body)},
		})
		for target := range page.OutLinks {
			addEdge(WikiGraphAPIEdge{Source: page.Path, Target: target, Kind: "wikilink", Relation: string(codegraph.RelWikiLink), Confidence: "EXTRACTED", ConfidenceScore: 1, Evidence: []string{page.Path}})
		}
	}

	manifest, err := loadSourceManifestFile(projectPath)
	if err != nil {
		return WikiGraphAPIResult{}, err
	}
	for key, entry := range manifest.Sources {
		rawPath := filepath.ToSlash(firstNonEmptyString(entry.ContentPath, entry.RawPath))
		if rawPath == "" {
			continue
		}
		addNode(WikiGraphAPINode{
			ID: rawPath, Path: rawPath, Title: firstNonEmptyString(entry.Title, filepath.Base(rawPath)),
			Label: firstNonEmptyString(entry.Title, filepath.Base(rawPath)), Type: "raw-source", Kind: string(codegraph.NodeRawSource), Domain: "source",
			SourceRef: rawPath, Props: map[string]any{"manifest_key": key, "archive_path": entry.ArchivePath, "sha256": entry.SHA256},
		})
	}
	for _, pagePath := range wikiPaths {
		for _, source := range wikiGraph.Nodes[pagePath].Sources {
			source = filepath.ToSlash(source)
			if _, ok := nodes[source]; !ok {
				addNode(WikiGraphAPINode{ID: source, Path: source, Title: filepath.Base(source), Label: filepath.Base(source), Type: "raw-source", Kind: string(codegraph.NodeRawSource), Domain: "source", SourceRef: source})
			}
			addEdge(WikiGraphAPIEdge{Source: pagePath, Target: source, Kind: "derived_from", Relation: string(codegraph.RelDerivedFrom), Confidence: "EXTRACTED", ConfidenceScore: 1, Evidence: []string{pagePath}})
		}
	}
	addSharedSourceRelations(wikiGraph, addEdge)

	snapshots, snapshotPaths, err := loadLatestCodeSnapshots(projectPath)
	if err != nil {
		return WikiGraphAPIResult{}, err
	}
	graphifySnapshots, graphifyPaths, err := loadSupplementalGraphifySnapshots(projectPath)
	if err != nil {
		return WikiGraphAPIResult{}, err
	}
	snapshots = append(snapshots, graphifySnapshots...)
	snapshotPaths = append(snapshotPaths, graphifyPaths...)
	for index, snapshot := range snapshots {
		sourcePath := snapshotPaths[index]
		for _, node := range snapshot.Nodes {
			addNode(WikiGraphAPINode{
				ID: node.ID, Path: node.SourceFile, Title: node.Label, Label: node.Label,
				Type: string(node.Kind), Kind: string(node.Kind), Domain: "code", ScopeID: snapshot.RepoID,
				Community: node.Community, SourceRef: sourcePath + "#node/" + node.ID, Props: copyAnyMap(node.Props),
			})
		}
		for _, edge := range snapshot.Edges {
			addEdge(WikiGraphAPIEdge{
				Source: edge.Source, Target: edge.Target, Kind: strings.ToLower(string(edge.Relation)), Relation: string(edge.Relation),
				Confidence: edge.Confidence, ConfidenceScore: edge.ConfidenceScore, Weight: edge.Weight,
				Evidence: append([]string(nil), edge.Evidence...), Props: copyAnyMap(edge.Props),
			})
		}
	}
	if err := addWikiCodeDocumentationEdges(projectPath, nodes, addEdge); err != nil {
		return WikiGraphAPIResult{}, err
	}
	for _, edge := range loadLLMSemanticRelations(projectPath) {
		if _, sourceOK := nodes[edge.Source]; sourceOK {
			if _, targetOK := nodes[edge.Target]; targetOK {
				addEdge(edge)
			}
		}
	}

	result := WikiGraphAPIResult{
		Nodes: []WikiGraphAPINode{},
		Edges: []WikiGraphAPIEdge{},
		Stats: ProjectGraphStats{ByDomain: map[string]int{}, ByKind: map[string]int{}},
	}
	for _, node := range nodes {
		result.Nodes = append(result.Nodes, node)
		result.Stats.ByDomain[node.Domain]++
		result.Stats.ByKind[node.Kind]++
	}
	for _, edge := range edges {
		result.Edges = append(result.Edges, edge)
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].ID < result.Nodes[j].ID })
	sort.Slice(result.Edges, func(i, j int) bool { return result.Edges[i].ID < result.Edges[j].ID })
	degrees := map[string][2]int{}
	for _, edge := range result.Edges {
		source := degrees[edge.Source]
		source[1]++
		degrees[edge.Source] = source
		target := degrees[edge.Target]
		target[0]++
		degrees[edge.Target] = target
	}
	for index := range result.Nodes {
		result.Nodes[index].InDegree = degrees[result.Nodes[index].ID][0]
		result.Nodes[index].OutDegree = degrees[result.Nodes[index].ID][1]
	}
	result.Stats.TotalNodes = len(result.Nodes)
	result.Stats.TotalEdges = len(result.Edges)
	return result, nil
}

func unifiedGraphInputSignature(projectPath string) uint64 {
	h := fnv.New64a()
	add := func(filename string, info os.FileInfo) {
		_, _ = h.Write([]byte(filepath.ToSlash(filename)))
		_, _ = h.Write([]byte(fmt.Sprintf("\x00%d\x00%d\x00", info.Size(), info.ModTime().UnixNano())))
	}
	for _, filename := range []string{
		filepath.Join(projectPath, ".kbcore", "source-manifest.json"),
		filepath.Join(projectPath, ".kbcore", "relations.json"),
	} {
		if info, err := os.Stat(filename); err == nil {
			add(filename, info)
		}
	}
	for _, root := range []string{filepath.Join(projectPath, "wiki"), filepath.Join(projectPath, "raw", "code-graphs")} {
		_ = filepath.WalkDir(root, func(filename string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, infoErr := d.Info(); infoErr == nil {
				add(filename, info)
			}
			return nil
		})
	}
	latestRoot := filepath.Join(projectPath, ".kbcore", "graph-snapshots", "code")
	_ = filepath.WalkDir(latestRoot, func(filename string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "latest.json" {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			add(filename, info)
		}
		data, readErr := os.ReadFile(filename)
		if readErr == nil {
			var pointer codeSnapshotPointer
			if json.Unmarshal(data, &pointer) == nil && pointer.SnapshotPath != "" {
				graphPath := filepath.Join(projectPath, filepath.FromSlash(pointer.SnapshotPath))
				if info, statErr := os.Stat(graphPath); statErr == nil {
					add(graphPath, info)
				}
			}
		}
		return nil
	})
	return h.Sum64()
}

func QueryProjectGraph(query ProjectGraphQuery) (WikiGraphAPIResult, error) {
	full, err := BuildUnifiedProjectGraph(query.ProjectPath)
	if err != nil {
		return WikiGraphAPIResult{}, err
	}
	if query.Depth < 0 {
		query.Depth = 0
	}
	if query.Depth == 0 && (len(query.SeedIDs) > 0 || strings.TrimSpace(query.Query) != "") {
		query.Depth = 1
	}
	if query.Limit <= 0 {
		query.Limit = 200
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}
	domainFilter := stringSet(query.Domains)
	kindFilter := stringSet(query.Kinds)
	relationFilter := stringSet(query.Relations)
	confidenceFilter := stringSet(query.Confidence)
	eligible := map[string]WikiGraphAPINode{}
	for _, node := range full.Nodes {
		if len(domainFilter) > 0 && !domainFilter[strings.ToLower(node.Domain)] {
			continue
		}
		if len(kindFilter) > 0 && !kindFilter[strings.ToLower(node.Kind)] {
			continue
		}
		eligible[node.ID] = node
	}
	var filteredEdges []WikiGraphAPIEdge
	adjacency := map[string][]string{}
	for _, edge := range full.Edges {
		if _, ok := eligible[edge.Source]; !ok {
			continue
		}
		if _, ok := eligible[edge.Target]; !ok {
			continue
		}
		if len(relationFilter) > 0 && !relationFilter[strings.ToLower(edge.Relation)] {
			continue
		}
		if len(confidenceFilter) > 0 && !confidenceFilter[strings.ToLower(edge.Confidence)] {
			continue
		}
		if query.MinConfidenceScore > 0 && edge.ConfidenceScore < query.MinConfidenceScore {
			continue
		}
		filteredEdges = append(filteredEdges, edge)
		adjacency[edge.Source] = append(adjacency[edge.Source], edge.Target)
		adjacency[edge.Target] = append(adjacency[edge.Target], edge.Source)
	}
	seeds := resolveGraphSeeds(query, eligible)
	selected := map[string]bool{}
	var order []string
	if len(seeds) > 0 {
		type frontierItem struct{ id, depth string }
		queue := make([]frontierItem, 0, len(seeds))
		for _, seed := range seeds {
			queue = append(queue, frontierItem{id: seed, depth: "0"})
		}
		for len(queue) > 0 {
			item := queue[0]
			queue = queue[1:]
			if selected[item.id] {
				continue
			}
			depth, _ := strconv.Atoi(item.depth)
			selected[item.id] = true
			order = append(order, item.id)
			if depth >= query.Depth {
				continue
			}
			neighbors := append([]string(nil), adjacency[item.id]...)
			sort.Strings(neighbors)
			for _, neighbor := range neighbors {
				queue = append(queue, frontierItem{id: neighbor, depth: strconv.Itoa(depth + 1)})
			}
		}
	} else {
		for id := range eligible {
			order = append(order, id)
		}
		sort.Slice(order, func(i, j int) bool {
			a, b := eligible[order[i]], eligible[order[j]]
			if a.InDegree+a.OutDegree != b.InDegree+b.OutDegree {
				return a.InDegree+a.OutDegree > b.InDegree+b.OutDegree
			}
			return a.ID < b.ID
		})
	}
	offset, _ := strconv.Atoi(query.Cursor)
	if offset < 0 || offset > len(order) {
		offset = 0
	}
	end := offset + query.Limit
	if end > len(order) {
		end = len(order)
	}
	pageIDs := map[string]bool{}
	result := WikiGraphAPIResult{Nodes: []WikiGraphAPINode{}, Edges: []WikiGraphAPIEdge{}, Stats: full.Stats}
	for _, id := range order[offset:end] {
		pageIDs[id] = true
		result.Nodes = append(result.Nodes, eligible[id])
	}
	for _, edge := range filteredEdges {
		if pageIDs[edge.Source] && pageIDs[edge.Target] {
			result.Edges = append(result.Edges, edge)
		}
	}
	if end < len(order) {
		result.Truncated = true
		result.NextCursor = strconv.Itoa(end)
	}
	return result, nil
}

func RefreshRelationsArtifact(projectPath string) error {
	relationsArtifactMu.Lock()
	defer relationsArtifactMu.Unlock()
	graph, err := BuildUnifiedProjectGraph(projectPath)
	if err != nil {
		return err
	}
	semantic := map[string]bool{
		string(codegraph.RelWikiLink): true, string(codegraph.RelDerivedFrom): true,
		string(codegraph.RelDocuments): true, string(codegraph.RelReferences): true,
		string(codegraph.RelCites): true, string(codegraph.RelSupports): true,
		string(codegraph.RelContradicts): true, string(codegraph.RelRelatedTo): true,
	}
	artifact := relationsArtifact{Version: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), GeneratedBy: "knowledge-core", Edges: []WikiGraphAPIEdge{}}
	for _, edge := range graph.Edges {
		if semantic[edge.Relation] {
			artifact.Edges = append(artifact.Edges, edge)
		}
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(projectPath, ".kbcore"), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(projectPath, ".kbcore", "relations.json"), data)
}

func loadLatestCodeSnapshots(projectPath string) ([]codegraph.Snapshot, []string, error) {
	root := filepath.Join(projectPath, ".kbcore", "graph-snapshots", "code")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var snapshots []codegraph.Snapshot
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pointerData, err := os.ReadFile(filepath.Join(root, entry.Name(), "latest.json"))
		if err != nil {
			continue
		}
		var pointer codeSnapshotPointer
		if json.Unmarshal(pointerData, &pointer) != nil || pointer.SnapshotPath == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(pointer.SnapshotPath)))
		if err != nil {
			continue
		}
		var snapshot codegraph.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, nil, err
		}
		snapshots = append(snapshots, snapshot)
		paths = append(paths, pointer.SnapshotPath)
	}
	return snapshots, paths, nil
}

func loadSupplementalGraphifySnapshots(projectPath string) ([]codegraph.Snapshot, []string, error) {
	root := filepath.Join(projectPath, "raw", "code-graphs")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil, nil
	}
	var snapshots []codegraph.Snapshot
	var paths []string
	err := filepath.WalkDir(root, func(filename string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Name() != "graph.json" {
			return nil
		}
		rel, err := filepath.Rel(projectPath, filename)
		if err != nil {
			return err
		}
		repoRel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		repoID := strings.Split(filepath.ToSlash(repoRel), "/")[0]
		report := filepath.Join(filepath.Dir(filename), "GRAPH_REPORT.md")
		if _, err := os.Stat(report); err != nil {
			report = ""
		}
		snapshot, err := codegraph.ImportGraphify(repoID, "", filename, report)
		if err != nil {
			return err
		}
		remap := map[string]string{}
		for index := range snapshot.Nodes {
			rawID := snapshot.Nodes[index].ID
			id := core.StableID("graphify", repoID, rawID)
			remap[rawID] = id
			snapshot.Nodes[index].ID = id
			if snapshot.Nodes[index].Props == nil {
				snapshot.Nodes[index].Props = map[string]any{}
			}
			snapshot.Nodes[index].Props["raw_id"] = rawID
		}
		for index := range snapshot.Edges {
			snapshot.Edges[index].Source = firstNonEmptyString(remap[snapshot.Edges[index].Source], core.StableID("graphify", repoID, snapshot.Edges[index].Source))
			snapshot.Edges[index].Target = firstNonEmptyString(remap[snapshot.Edges[index].Target], core.StableID("graphify", repoID, snapshot.Edges[index].Target))
		}
		snapshots = append(snapshots, snapshot)
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	return snapshots, paths, err
}

func addSharedSourceRelations(graph WikiGraph, add func(WikiGraphAPIEdge)) {
	bySource := map[string][]string{}
	for path, page := range graph.Nodes {
		for _, source := range page.Sources {
			bySource[filepath.ToSlash(source)] = append(bySource[filepath.ToSlash(source)], path)
		}
	}
	for source, pages := range bySource {
		sort.Strings(pages)
		if len(pages) > 30 {
			pages = pages[:30]
		}
		for i := 0; i < len(pages); i++ {
			for j := i + 1; j < len(pages); j++ {
				add(WikiGraphAPIEdge{Source: pages[i], Target: pages[j], Kind: "related_to", Relation: string(codegraph.RelRelatedTo), Confidence: "INFERRED", ConfidenceScore: 0.8, Evidence: []string{source}, Props: map[string]any{"reason": "shared_source"}})
			}
		}
	}
}

func addWikiCodeDocumentationEdges(projectPath string, nodes map[string]WikiGraphAPINode, add func(WikiGraphAPIEdge)) error {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return err
	}
	for _, page := range pages {
		repo := frontmatterString(page.Frontmatter, "code_repo")
		if repo == "" {
			continue
		}
		for _, symbolID := range frontmatterStrings(page.Frontmatter, "symbol_ids") {
			node, ok := nodes[symbolID]
			if !ok || node.Domain != "code" || node.ScopeID != repo {
				continue
			}
			add(WikiGraphAPIEdge{Source: page.Path, Target: symbolID, Kind: "documents", Relation: string(codegraph.RelDocuments), Confidence: "EXTRACTED", ConfidenceScore: 1, Evidence: []string{page.Path}})
		}
	}
	return nil
}

func loadLLMSemanticRelations(projectPath string) []WikiGraphAPIEdge {
	data, err := os.ReadFile(filepath.Join(projectPath, ".kbcore", "relations.json"))
	if err != nil {
		return nil
	}
	var artifact relationsArtifact
	if json.Unmarshal(data, &artifact) != nil {
		return nil
	}
	var out []WikiGraphAPIEdge
	for _, edge := range artifact.Edges {
		if generatedBy, _ := edge.Props["generated_by"].(string); generatedBy == "llm" {
			out = append(out, edge)
		}
	}
	return out
}

func resolveGraphSeeds(query ProjectGraphQuery, eligible map[string]WikiGraphAPINode) []string {
	seen := map[string]bool{}
	var seeds []string
	for _, id := range query.SeedIDs {
		if _, ok := eligible[id]; ok && !seen[id] {
			seen[id] = true
			seeds = append(seeds, id)
		}
	}
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	if needle != "" {
		var matches []WikiGraphAPINode
		for _, node := range eligible {
			haystack := strings.ToLower(strings.Join([]string{node.ID, node.Path, node.Label, node.Title, node.Kind}, " "))
			if strings.Contains(haystack, needle) {
				matches = append(matches, node)
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			a, b := matches[i], matches[j]
			if a.InDegree+a.OutDegree != b.InDegree+b.OutDegree {
				return a.InDegree+a.OutDegree > b.InDegree+b.OutDegree
			}
			return a.ID < b.ID
		})
		for _, node := range matches {
			if len(seeds) >= 10 {
				break
			}
			if !seen[node.ID] {
				seen[node.ID] = true
				seeds = append(seeds, node.ID)
			}
		}
	}
	sort.Strings(seeds)
	return seeds
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func frontmatterString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func frontmatterStrings(values map[string]any, key string) []string {
	value := values[key]
	switch typed := value.(type) {
	case string:
		var out []string
		for _, item := range strings.Split(typed, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []any:
		var out []string
		for _, item := range typed {
			if value := strings.TrimSpace(fmt.Sprint(item)); value != "" {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range additions {
		if value != "" && !seen[value] {
			values = append(values, value)
			seen[value] = true
		}
	}
	return values
}

func copyAnyMap(values map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}
