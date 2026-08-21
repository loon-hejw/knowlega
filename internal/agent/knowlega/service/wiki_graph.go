package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type WikiGraphNode struct {
	Path     string
	Title    string
	Type     string
	Sources  []string
	OutLinks map[string]bool
	InLinks  map[string]bool
	Body     string
}

type WikiGraph struct {
	Nodes map[string]*WikiGraphNode
}

func SearchWikiGraphDocuments(projectPath, q string, seedPaths []string, limit int, maxRunes int) ([]KnowledgeDocument, error) {
	if limit <= 0 {
		limit = 5
	}
	graph, err := BuildWikiGraph(projectPath)
	if err != nil {
		return nil, err
	}
	if len(graph.Nodes) == 0 {
		return nil, nil
	}
	seeds := graphSeedNodes(graph, q, seedPaths)
	type scored struct {
		path  string
		score float64
	}
	scores := map[string]float64{}
	for _, seed := range seeds {
		for path, node := range graph.Nodes {
			if path == seed.Path || isAggregateWikiPath(path) {
				continue
			}
			score := wikiGraphRelevance(seed, node, graph)
			if score <= 0 {
				continue
			}
			scores[path] += score
		}
	}
	if len(scores) == 0 && strings.TrimSpace(q) != "" {
		terms := knowledgeQueryTerms(q)
		for path, node := range graph.Nodes {
			if isAggregateWikiPath(path) {
				continue
			}
			score := scoreKnowledgeContent(strings.Join([]string{node.Path, node.Title, node.Type, node.Body}, "\n"), terms, node.Path, "wiki-graph")
			if score > 0 {
				scores[path] = float64(score)
			}
		}
	}
	var ranked []scored
	for path, score := range scores {
		ranked = append(ranked, scored{path: path, score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].path < ranked[j].path
		}
		return ranked[i].score > ranked[j].score
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	docs := make([]KnowledgeDocument, 0, len(ranked))
	for _, item := range ranked {
		node := graph.Nodes[item.path]
		content, err := readProjectText(projectPath, item.path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		docs = append(docs, KnowledgeDocument{
			Path:    item.path,
			Title:   node.Title,
			Kind:    "wiki-graph",
			Aliases: aliasesFromMarkdown(content),
			Content: tailRunes(content, maxRunes),
		})
	}
	return docs, nil
}

func BuildWikiGraph(projectPath string) (WikiGraph, error) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if os.IsNotExist(err) {
		return WikiGraph{Nodes: map[string]*WikiGraphNode{}}, nil
	}
	if err != nil {
		return WikiGraph{}, err
	}
	nodes := make(map[string]*WikiGraphNode, len(pages))
	resolver := map[string]string{}
	for _, page := range pages {
		if isAggregateWikiPath(page.Path) {
			continue
		}
		node := &WikiGraphNode{
			Path:     page.Path,
			Title:    page.Title,
			Type:     page.Type,
			Sources:  append([]string(nil), page.Sources...),
			OutLinks: map[string]bool{},
			InLinks:  map[string]bool{},
			Body:     page.Body,
		}
		nodes[page.Path] = node
		for _, key := range wikiGraphKeys(page) {
			resolver[key] = page.Path
		}
	}
	for _, node := range nodes {
		for _, link := range extractWikiMarkdownLinks(node.Body) {
			target := resolveWikiGraphLink(link, resolver)
			if target == "" || target == node.Path {
				continue
			}
			node.OutLinks[target] = true
		}
	}
	for path, node := range nodes {
		for target := range node.OutLinks {
			if targetNode := nodes[target]; targetNode != nil {
				targetNode.InLinks[path] = true
			}
		}
	}
	return WikiGraph{Nodes: nodes}, nil
}

func graphSeedNodes(graph WikiGraph, q string, seedPaths []string) []*WikiGraphNode {
	var seeds []*WikiGraphNode
	seen := map[string]bool{}
	for _, path := range seedPaths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if node := graph.Nodes[path]; node != nil && !seen[path] {
			seeds = append(seeds, node)
			seen[path] = true
		}
	}
	if len(seeds) > 0 || strings.TrimSpace(q) == "" {
		return seeds
	}
	terms := knowledgeQueryTerms(q)
	type scored struct {
		node  *WikiGraphNode
		score int
	}
	var ranked []scored
	for _, node := range graph.Nodes {
		score := scoreKnowledgeContent(strings.Join([]string{node.Path, node.Title, node.Type, node.Body}, "\n"), terms, node.Path, "wiki-graph")
		if score > 0 {
			ranked = append(ranked, scored{node: node, score: score})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].node.Path < ranked[j].node.Path
		}
		return ranked[i].score > ranked[j].score
	})
	for i := 0; i < len(ranked) && i < 3; i++ {
		seeds = append(seeds, ranked[i].node)
	}
	return seeds
}

func wikiGraphRelevance(a, b *WikiGraphNode, graph WikiGraph) float64 {
	if a == nil || b == nil || a.Path == b.Path {
		return 0
	}
	var score float64
	if a.OutLinks[b.Path] || a.InLinks[b.Path] || b.OutLinks[a.Path] || b.InLinks[a.Path] {
		score += 3.0
	}
	score += 4.0 * sourceOverlapScore(a.Sources, b.Sources)
	score += 1.5 * commonNeighborScore(a, b, graph)
	score += typeAffinityScore(a.Type, b.Type)
	return score
}

func sourceOverlapScore(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, source := range a {
		if strings.TrimSpace(source) != "" {
			set[source] = true
		}
	}
	var matches int
	for _, source := range b {
		if set[source] {
			matches++
		}
	}
	if matches == 0 {
		return 0
	}
	denom := len(a)
	if len(b) > denom {
		denom = len(b)
	}
	return float64(matches) / float64(denom)
}

func commonNeighborScore(a, b *WikiGraphNode, graph WikiGraph) float64 {
	neighborsA := wikiGraphNeighbors(a)
	neighborsB := wikiGraphNeighbors(b)
	var score float64
	for path := range neighborsA {
		if !neighborsB[path] {
			continue
		}
		degree := wikiGraphDegree(graph.Nodes[path])
		if degree <= 0 {
			degree = 1
		}
		score += 1.0 / float64(degree)
	}
	return score
}

func typeAffinityScore(a, b string) float64 {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 0.8
	}
	if (a == "entity" && b == "concept") || (a == "concept" && b == "entity") {
		return 1.2
	}
	if a == "synthesis" || b == "synthesis" {
		return 1.0
	}
	if a == "source-summary" || b == "source-summary" || a == "source" || b == "source" {
		return 0.8
	}
	return 0.4
}

func wikiGraphNeighbors(node *WikiGraphNode) map[string]bool {
	out := map[string]bool{}
	if node == nil {
		return out
	}
	for path := range node.OutLinks {
		out[path] = true
	}
	for path := range node.InLinks {
		out[path] = true
	}
	return out
}

func wikiGraphDegree(node *WikiGraphNode) int {
	if node == nil {
		return 0
	}
	return len(wikiGraphNeighbors(node))
}

func wikiGraphKeys(page core.WikiPage) []string {
	base := strings.TrimSuffix(filepath.Base(page.Path), filepath.Ext(page.Path))
	values := []string{page.Path, base, page.Title, core.Slug(page.Title)}
	var keys []string
	for _, value := range values {
		value = canonicalWikiGraphKey(value)
		if value != "" {
			keys = append(keys, value)
		}
	}
	return keys
}

func resolveWikiGraphLink(link string, resolver map[string]string) string {
	link = strings.TrimSpace(strings.Split(strings.Split(link, "|")[0], "#")[0])
	if link == "" || strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		return ""
	}
	candidates := []string{link}
	if !strings.HasPrefix(link, "wiki/") && strings.HasSuffix(link, ".md") {
		candidates = append(candidates, filepath.ToSlash(filepath.Join("wiki", link)))
	}
	if !strings.HasSuffix(link, ".md") && strings.Contains(link, "/") {
		candidates = append(candidates, link+".md")
	}
	for _, candidate := range candidates {
		if path := resolver[canonicalWikiGraphKey(candidate)]; path != "" {
			return path
		}
	}
	return ""
}

func canonicalWikiGraphKey(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, ".md"))
	value = strings.TrimPrefix(value, "wiki/")
	value = strings.TrimPrefix(value, "./")
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.Join(strings.Fields(value), "-")
	return value
}

func formatWikiGraphObservation(docs []KnowledgeDocument) string {
	if len(docs) == 0 {
		return "wiki graph returned 0 related page(s)"
	}
	paths := make([]string, 0, len(docs))
	for _, doc := range docs {
		paths = append(paths, fmt.Sprintf("%s title=%q", doc.Path, doc.Title))
	}
	return fmt.Sprintf("wiki graph returned %d related page(s): %s", len(docs), strings.Join(paths, "; "))
}
