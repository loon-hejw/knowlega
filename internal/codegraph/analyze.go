package codegraph

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
)

// ApplyCommunities performs deterministic modularity local-moving followed by
// a connectivity refinement. This is Leiden-aligned behavior: locally optimal
// communities are refined so a community cannot contain disconnected pieces.
func ApplyCommunities(snapshot *Snapshot) {
	if snapshot == nil || len(snapshot.Nodes) == 0 {
		return
	}
	nodeByID := map[string]*Node{}
	eligible := map[string]bool{}
	for index := range snapshot.Nodes {
		node := &snapshot.Nodes[index]
		nodeByID[node.ID] = node
		if node.Kind != NodeCommunity && node.Kind != NodeProcess && !boolProp(node.Props, "external") {
			eligible[node.ID] = true
		}
	}
	adjacency := map[string]map[string]float64{}
	degree := map[string]float64{}
	for _, edge := range snapshot.Edges {
		if !eligible[edge.Source] || !eligible[edge.Target] || edge.Source == edge.Target {
			continue
		}
		weight := communityEdgeWeight(edge)
		if weight <= 0 {
			continue
		}
		if adjacency[edge.Source] == nil {
			adjacency[edge.Source] = map[string]float64{}
		}
		if adjacency[edge.Target] == nil {
			adjacency[edge.Target] = map[string]float64{}
		}
		adjacency[edge.Source][edge.Target] += weight
		adjacency[edge.Target][edge.Source] += weight
		degree[edge.Source] += weight
		degree[edge.Target] += weight
	}
	ids := make([]string, 0, len(eligible))
	for id := range eligible {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	community := map[string]string{}
	total := map[string]float64{}
	var m2 float64
	for _, id := range ids {
		community[id] = id
		total[id] = degree[id]
		m2 += degree[id]
	}
	if m2 == 0 {
		return
	}
	for pass := 0; pass < 20; pass++ {
		moved := false
		for _, id := range ids {
			current := community[id]
			ki := degree[id]
			total[current] -= ki
			weightsByCommunity := map[string]float64{}
			for neighbor, weight := range adjacency[id] {
				weightsByCommunity[community[neighbor]] += weight
			}
			best := current
			bestGain := weightsByCommunity[current] - total[current]*ki/m2
			candidates := make([]string, 0, len(weightsByCommunity))
			for candidate := range weightsByCommunity {
				candidates = append(candidates, candidate)
			}
			sort.Strings(candidates)
			for _, candidate := range candidates {
				gain := weightsByCommunity[candidate] - total[candidate]*ki/m2
				if gain > bestGain+1e-12 || (math.Abs(gain-bestGain) <= 1e-12 && candidate < best) {
					best, bestGain = candidate, gain
				}
			}
			community[id] = best
			total[best] += ki
			if best != current {
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	community = refineDisconnectedCommunities(ids, community, adjacency)
	groups := map[string][]string{}
	for _, id := range ids {
		groups[community[id]] = append(groups[community[id]], id)
	}
	groupKeys := make([]string, 0, len(groups))
	for key := range groups {
		if len(groups[key]) >= 2 {
			groupKeys = append(groupKeys, key)
		}
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groups[groupKeys[i]], groups[groupKeys[j]]
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return strings.Join(a, "\x00") < strings.Join(b, "\x00")
	})
	assignments := map[string]string{}
	var communityNodes []Node
	var membershipEdges []Edge
	for _, key := range groupKeys {
		members := groups[key]
		sort.Strings(members)
		communityID := core.StableID(snapshot.RepoID, "community", strings.Join(members, ","))
		label, topKinds := communityLabel(members, nodeByID)
		cohesion := communityCohesion(members, adjacency)
		snapshot.Communities = append(snapshot.Communities, Community{ID: communityID, Label: label, Members: append([]string(nil), members...), Cohesion: cohesion, TopKinds: topKinds})
		communityNodes = append(communityNodes, Node{
			ID: communityID, Kind: NodeCommunity, Label: label,
			Props: map[string]any{"member_count": len(members), "cohesion": cohesion, "top_kinds": topKinds},
		})
		for _, memberID := range members {
			assignments[memberID] = communityID
			membershipEdges = append(membershipEdges, Edge{
				Source: memberID, Target: communityID, Relation: RelMemberOf,
				Confidence: "EXTRACTED", ConfidenceScore: 1, Weight: 1,
			})
		}
	}
	for index := range snapshot.Nodes {
		if communityID := assignments[snapshot.Nodes[index].ID]; communityID != "" {
			snapshot.Nodes[index].Community = communityID
		}
	}
	snapshot.Nodes = append(snapshot.Nodes, communityNodes...)
	snapshot.Edges = append(snapshot.Edges, membershipEdges...)
	sortSnapshot(snapshot)
}

func ReconcileCommunityIDs(previous, current *Snapshot) {
	if previous == nil || current == nil || len(previous.Communities) == 0 {
		return
	}
	replacements := map[string]string{}
	used := map[string]bool{}
	for index := range current.Communities {
		candidate := &current.Communities[index]
		bestID := ""
		bestScore := 0.0
		for _, old := range previous.Communities {
			if used[old.ID] {
				continue
			}
			score := jaccard(candidate.Members, old.Members)
			if score > bestScore || (score == bestScore && old.ID < bestID) {
				bestID, bestScore = old.ID, score
			}
		}
		if bestScore >= 0.5 && bestID != "" && bestID != candidate.ID {
			replacements[candidate.ID] = bestID
			candidate.ID = bestID
			used[bestID] = true
		}
	}
	if len(replacements) == 0 {
		return
	}
	for index := range current.Nodes {
		if replacement := replacements[current.Nodes[index].ID]; replacement != "" {
			current.Nodes[index].ID = replacement
		}
		if replacement := replacements[current.Nodes[index].Community]; replacement != "" {
			current.Nodes[index].Community = replacement
		}
	}
	for index := range current.Edges {
		if replacement := replacements[current.Edges[index].Source]; replacement != "" {
			current.Edges[index].Source = replacement
		}
		if replacement := replacements[current.Edges[index].Target]; replacement != "" {
			current.Edges[index].Target = replacement
		}
	}
	sortSnapshot(current)
}

func DetectProcesses(snapshot *Snapshot, maxDepth, branchLimit, minSteps, maxSteps int) {
	if snapshot == nil {
		return
	}
	if maxDepth <= 0 {
		maxDepth = 10
	}
	if branchLimit <= 0 {
		branchLimit = 4
	}
	if minSteps <= 0 {
		minSteps = 3
	}
	if maxSteps <= 0 {
		maxSteps = 75
	}
	nodes := map[string]Node{}
	callOut := map[string][]string{}
	callIn := map[string]int{}
	routeHandlers := map[string]bool{}
	for _, node := range snapshot.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range snapshot.Edges {
		switch edge.Relation {
		case RelCalls:
			if edge.ConfidenceScore >= 0.5 || edge.Confidence == "EXTRACTED" {
				callOut[edge.Source] = append(callOut[edge.Source], edge.Target)
				callIn[edge.Target]++
			}
		case RelHandlesRoute, RelHandlesTool:
			routeHandlers[edge.Source] = true
		}
	}
	var entries []string
	for id, targets := range callOut {
		if len(targets) > 0 && (callIn[id] == 0 || routeHandlers[id]) {
			entries = append(entries, id)
		}
		sort.Strings(callOut[id])
	}
	sort.Slice(entries, func(i, j int) bool {
		if routeHandlers[entries[i]] != routeHandlers[entries[j]] {
			return routeHandlers[entries[i]]
		}
		return nodes[entries[i]].Label < nodes[entries[j]].Label
	})
	if len(entries) > 25 {
		entries = entries[:25]
	}
	for _, entry := range entries {
		steps := processTraversal(entry, callOut, maxDepth, branchLimit, maxSteps)
		if len(steps) < minSteps {
			continue
		}
		processID := core.StableID(snapshot.RepoID, "process", entry)
		label := nodes[entry].Label + " flow"
		snapshot.Processes = append(snapshot.Processes, Process{ID: processID, Label: label, EntryID: entry, Steps: steps})
		snapshot.Nodes = append(snapshot.Nodes, Node{ID: processID, Kind: NodeProcess, Label: label, SourceFile: nodes[entry].SourceFile, Props: map[string]any{"entry_id": entry, "step_count": len(steps)}})
		for order, step := range steps {
			snapshot.Edges = append(snapshot.Edges, Edge{
				Source: processID, Target: step, Relation: RelStepInProcess,
				Confidence: "EXTRACTED", ConfidenceScore: 1, Weight: 1,
				Props: map[string]any{"order": order + 1},
			})
		}
	}
	sortSnapshot(snapshot)
}

func BuildSnapshotInsights(snapshot *Snapshot) {
	if snapshot == nil {
		return
	}
	nodes := map[string]Node{}
	degree := map[string]int{}
	for _, node := range snapshot.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range snapshot.Edges {
		if edge.Relation == RelMemberOf || edge.Relation == RelStepInProcess || edge.Relation == RelContains {
			continue
		}
		degree[edge.Source]++
		degree[edge.Target]++
	}
	type ranked struct {
		id     string
		degree int
	}
	var ranking []ranked
	for id, value := range degree {
		if nodes[id].Kind != NodeCommunity && nodes[id].Kind != NodeProcess {
			ranking = append(ranking, ranked{id: id, degree: value})
		}
	}
	sort.Slice(ranking, func(i, j int) bool {
		if ranking[i].degree == ranking[j].degree {
			return nodes[ranking[i].id].Label < nodes[ranking[j].id].Label
		}
		return ranking[i].degree > ranking[j].degree
	})
	threshold := 5
	if len(ranking) > 0 {
		p95 := ranking[int(float64(len(ranking)-1)*0.05)].degree
		if p95 > threshold {
			threshold = p95
		}
	}
	for _, item := range ranking {
		if item.degree < threshold || len(snapshot.Insights.GodNodes) >= 20 {
			break
		}
		node := nodes[item.id]
		snapshot.Insights.GodNodes = append(snapshot.Insights.GodNodes, InsightItem{NodeID: item.id, Label: node.Label, Reason: fmt.Sprintf("高连接节点（度数 %d），变更时应重点检查依赖面。", item.degree), Score: float64(item.degree)})
	}
	for _, edge := range snapshot.Edges {
		source, target := nodes[edge.Source], nodes[edge.Target]
		if source.Community == "" || target.Community == "" || source.Community == target.Community {
			continue
		}
		if edge.Relation != RelCalls && edge.Relation != RelImplements && edge.Relation != RelImports {
			continue
		}
		if len(snapshot.Insights.SurprisingConnections) >= 30 {
			break
		}
		snapshot.Insights.SurprisingConnections = append(snapshot.Insights.SurprisingConnections, InsightItem{
			EdgeID: core.StableID(snapshot.RepoID, edge.Source, edge.Target, string(edge.Relation)),
			Label:  source.Label + " → " + target.Label,
			Reason: "跨社区的精确依赖，可能是架构耦合点或专题 Wiki 的种子。",
			Score:  edge.ConfidenceScore,
		})
	}
	for index, item := range snapshot.Insights.GodNodes {
		if index >= 8 {
			break
		}
		snapshot.Insights.SuggestedQuestions = append(snapshot.Insights.SuggestedQuestions, fmt.Sprintf("%s 的主要调用者、下游流程和变更风险是什么？", item.Label))
	}
}

func RenderGraphReport(snapshot Snapshot) string {
	kinds := map[NodeKind]int{}
	relations := map[Relation]int{}
	for _, node := range snapshot.Nodes {
		kinds[node.Kind]++
	}
	for _, edge := range snapshot.Edges {
		relations[edge.Relation]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Graph Report: %s\n\n", snapshot.RepoID)
	fmt.Fprintf(&b, "- Source: `%s`\n- Commit: `%s`\n- Working tree dirty: `%t`\n- Indexed source SHA256: `%s`\n- Nodes: %d\n- Edges: %d\n- Communities: %d\n- Processes: %d\n\n", snapshot.Source, snapshot.Commit, snapshot.Dirty, snapshot.SourceSHA256, len(snapshot.Nodes), len(snapshot.Edges), len(snapshot.Communities), len(snapshot.Processes))
	b.WriteString("## Node Kinds\n\n")
	var kindNames []string
	for kind := range kinds {
		kindNames = append(kindNames, string(kind))
	}
	sort.Strings(kindNames)
	for _, kind := range kindNames {
		fmt.Fprintf(&b, "- %s: %d\n", kind, kinds[NodeKind(kind)])
	}
	b.WriteString("\n## Relations\n\n")
	var relationNames []string
	for relation := range relations {
		relationNames = append(relationNames, string(relation))
	}
	sort.Strings(relationNames)
	for _, relation := range relationNames {
		fmt.Fprintf(&b, "- %s: %d\n", relation, relations[Relation(relation)])
	}
	b.WriteString("\n## Communities\n\n")
	for _, community := range snapshot.Communities {
		fmt.Fprintf(&b, "- %s: %d members, cohesion %.3f\n", community.Label, len(community.Members), community.Cohesion)
	}
	b.WriteString("\n## God Nodes\n\n")
	for _, item := range snapshot.Insights.GodNodes {
		fmt.Fprintf(&b, "- %s: %s\n", item.Label, item.Reason)
	}
	b.WriteString("\n## Surprising Connections\n\n")
	for _, item := range snapshot.Insights.SurprisingConnections {
		fmt.Fprintf(&b, "- %s: %s\n", item.Label, item.Reason)
	}
	b.WriteString("\n## Suggested Questions\n\n")
	for _, question := range snapshot.Insights.SuggestedQuestions {
		fmt.Fprintf(&b, "- %s\n", question)
	}
	return b.String()
}

func refineDisconnectedCommunities(ids []string, communities map[string]string, adjacency map[string]map[string]float64) map[string]string {
	groups := map[string][]string{}
	for _, id := range ids {
		groups[communities[id]] = append(groups[communities[id]], id)
	}
	refined := map[string]string{}
	for key, members := range groups {
		allowed := map[string]bool{}
		for _, member := range members {
			allowed[member] = true
		}
		sort.Strings(members)
		component := 0
		for _, start := range members {
			if refined[start] != "" {
				continue
			}
			component++
			componentKey := fmt.Sprintf("%s#%04d", key, component)
			queue := []string{start}
			refined[start] = componentKey
			for len(queue) > 0 {
				current := queue[0]
				queue = queue[1:]
				var neighbors []string
				for neighbor := range adjacency[current] {
					if allowed[neighbor] && refined[neighbor] == "" {
						neighbors = append(neighbors, neighbor)
					}
				}
				sort.Strings(neighbors)
				for _, neighbor := range neighbors {
					refined[neighbor] = componentKey
					queue = append(queue, neighbor)
				}
			}
		}
	}
	return refined
}

func communityLabel(members []string, nodes map[string]*Node) (string, []string) {
	kinds := map[string]int{}
	labels := make([]string, 0, len(members))
	for _, id := range members {
		if node := nodes[id]; node != nil {
			kinds[string(node.Kind)]++
			if node.Kind != NodeFile && node.Kind != NodeFolder {
				labels = append(labels, node.Label)
			}
		}
	}
	sort.Strings(labels)
	if len(labels) > 3 {
		labels = labels[:3]
	}
	if len(labels) == 0 {
		labels = []string{"structural cluster"}
	}
	type kindCount struct {
		kind  string
		count int
	}
	var ranked []kindCount
	for kind, count := range kinds {
		ranked = append(ranked, kindCount{kind, count})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count == ranked[j].count {
			return ranked[i].kind < ranked[j].kind
		}
		return ranked[i].count > ranked[j].count
	})
	var topKinds []string
	for i := 0; i < len(ranked) && i < 3; i++ {
		topKinds = append(topKinds, ranked[i].kind)
	}
	return strings.Join(labels, " · "), topKinds
}

func communityCohesion(members []string, adjacency map[string]map[string]float64) float64 {
	if len(members) < 2 {
		return 0
	}
	set := map[string]bool{}
	for _, id := range members {
		set[id] = true
	}
	var internal, total float64
	for _, id := range members {
		for neighbor, weight := range adjacency[id] {
			total += weight
			if set[neighbor] {
				internal += weight
			}
		}
	}
	if total == 0 {
		return 0
	}
	return internal / total
}

func communityEdgeWeight(edge Edge) float64 {
	base := edge.Weight
	if base <= 0 {
		base = 1
	}
	switch edge.Relation {
	case RelCalls, RelImplements, RelHandlesRoute, RelHandlesTool:
		return 3 * base
	case RelImports, RelEmbeds, RelHasMethod:
		return 2 * base
	case RelDefines:
		return 0.8 * base
	case RelContains:
		return 0.2 * base
	case RelMemberOf, RelStepInProcess:
		return 0
	default:
		return base
	}
}

func processTraversal(entry string, adjacency map[string][]string, maxDepth, branchLimit, maxSteps int) []string {
	seen := map[string]bool{}
	type state struct {
		id    string
		depth int
	}
	queue := []state{{entry, 0}}
	var steps []string
	for len(queue) > 0 && len(steps) < maxSteps {
		current := queue[0]
		queue = queue[1:]
		if seen[current.id] || current.depth > maxDepth {
			continue
		}
		seen[current.id] = true
		steps = append(steps, current.id)
		targets := adjacency[current.id]
		if len(targets) > branchLimit {
			targets = targets[:branchLimit]
		}
		for _, target := range targets {
			queue = append(queue, state{target, current.depth + 1})
		}
	}
	return steps
}

func sortSnapshot(snapshot *Snapshot) {
	sort.Slice(snapshot.Nodes, func(i, j int) bool { return snapshot.Nodes[i].ID < snapshot.Nodes[j].ID })
	sort.Slice(snapshot.Edges, func(i, j int) bool {
		a := snapshot.Edges[i]
		b := snapshot.Edges[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Relation < b.Relation
	})
	sort.Slice(snapshot.Communities, func(i, j int) bool { return snapshot.Communities[i].ID < snapshot.Communities[j].ID })
	sort.Slice(snapshot.Processes, func(i, j int) bool { return snapshot.Processes[i].ID < snapshot.Processes[j].ID })
}

func boolProp(props map[string]any, key string) bool {
	value, _ := props[key].(bool)
	return value
}

func jaccard(a, b []string) float64 {
	set := map[string]bool{}
	for _, value := range a {
		set[value] = true
	}
	intersection := 0
	union := len(set)
	for _, value := range b {
		if set[value] {
			intersection++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
