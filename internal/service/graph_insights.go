package service

import (
	"sort"
)

type WikiGraphInsights struct {
	IsolatedPages         []WikiGraphInsight `json:"isolated_pages"`
	HubPages              []WikiGraphInsight `json:"hub_pages"`
	MissingSources        []WikiGraphInsight `json:"missing_sources"`
	BridgeCandidates      []WikiGraphInsight `json:"bridge_candidates"`
	GodNodes              []WikiGraphInsight `json:"god_nodes,omitempty"`
	SurprisingConnections []WikiGraphInsight `json:"surprising_connections,omitempty"`
	SuggestedQuestions    []string           `json:"suggested_questions,omitempty"`
}

type WikiGraphInsight struct {
	ID        string  `json:"id,omitempty"`
	Domain    string  `json:"domain,omitempty"`
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Type      string  `json:"type"`
	InDegree  int     `json:"in_degree"`
	OutDegree int     `json:"out_degree"`
	Reason    string  `json:"reason"`
	Query     string  `json:"query,omitempty"`
	Score     float64 `json:"score,omitempty"`
}

func BuildWikiGraphInsights(projectPath string) (WikiGraphInsights, error) {
	graph, err := BuildWikiGraph(projectPath)
	if err != nil {
		return WikiGraphInsights{}, err
	}
	nodes := make([]*WikiGraphNode, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return wikiGraphDegree(nodes[i]) > wikiGraphDegree(nodes[j])
	})
	var insights WikiGraphInsights
	for _, node := range nodes {
		inDegree := len(node.InLinks)
		outDegree := len(node.OutLinks)
		degree := inDegree + outDegree
		item := WikiGraphInsight{
			Path:      node.Path,
			Title:     node.Title,
			Type:      node.Type,
			InDegree:  inDegree,
			OutDegree: outDegree,
			Query:     node.Title,
		}
		if degree == 0 && len(insights.IsolatedPages) < 25 {
			item.Reason = "页面没有入链或出链，可能需要补充 wikilink 或合并到已有主题。"
			insights.IsolatedPages = append(insights.IsolatedPages, item)
		}
		if len(node.Sources) == 0 && node.Type != "index" && len(insights.MissingSources) < 25 {
			item.Reason = "页面缺少 sources frontmatter，后续回答难以追溯证据。"
			insights.MissingSources = append(insights.MissingSources, item)
		}
		if degree >= 4 && len(insights.HubPages) < 25 {
			item.Reason = "页面连接较多，是查询导航和概念整理的关键节点。"
			insights.HubPages = append(insights.HubPages, item)
		}
		if inDegree >= 2 && outDegree >= 2 && len(insights.BridgeCandidates) < 25 {
			item.Reason = "页面同时有多个入链和出链，适合检查是否需要综合页或专题研究。"
			insights.BridgeCandidates = append(insights.BridgeCandidates, item)
		}
	}
	snapshots, _, snapshotErr := loadLatestCodeSnapshots(projectPath)
	if snapshotErr != nil {
		return WikiGraphInsights{}, snapshotErr
	}
	for _, snapshot := range snapshots {
		nodeMap := snapshotNodeMap(snapshot)
		for _, item := range snapshot.Insights.GodNodes {
			node := nodeMap[item.NodeID]
			insights.GodNodes = append(insights.GodNodes, WikiGraphInsight{
				ID: item.NodeID, Domain: "code", Path: node.SourceFile, Title: item.Label,
				Type: string(node.Kind), Reason: item.Reason, Query: item.Label, Score: item.Score,
			})
		}
		for _, item := range snapshot.Insights.SurprisingConnections {
			insights.SurprisingConnections = append(insights.SurprisingConnections, WikiGraphInsight{
				ID: item.EdgeID, Domain: "code", Title: item.Label, Type: "cross-community", Reason: item.Reason, Query: item.Label, Score: item.Score,
			})
		}
		insights.SuggestedQuestions = append(insights.SuggestedQuestions, snapshot.Insights.SuggestedQuestions...)
	}
	return insights, nil
}
