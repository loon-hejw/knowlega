package codegraph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type graphifyJSON struct {
	Nodes []map[string]any `json:"nodes"`
	Edges []map[string]any `json:"edges"`
}

func ImportGraphify(repoID, repoPath, graphPath, reportPath string) (Snapshot, error) {
	data, err := os.ReadFile(graphPath)
	if err != nil {
		return Snapshot{}, err
	}
	var raw graphifyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{
		RepoID:     repoID,
		RepoPath:   repoPath,
		Source:     "graphify",
		ImportedAt: time.Now(),
	}
	for _, n := range raw.Nodes {
		id := stringField(n, "id")
		if id == "" {
			id = core.StableID(repoID, stringField(n, "label"), stringField(n, "source_file"))
		}
		snap.Nodes = append(snap.Nodes, Node{
			ID:         id,
			Kind:       graphifyNodeKind(n),
			Label:      firstNonEmpty(stringField(n, "label"), id),
			SourceFile: stringField(n, "source_file"),
			Community:  fmt.Sprint(n["community"]),
			Props:      n,
		})
	}
	for _, e := range raw.Edges {
		source := firstNonEmpty(stringField(e, "source"), stringField(e, "from"))
		target := firstNonEmpty(stringField(e, "target"), stringField(e, "to"))
		if source == "" || target == "" {
			continue
		}
		snap.Edges = append(snap.Edges, Edge{
			Source:     source,
			Target:     target,
			Relation:   graphifyRelation(e),
			Confidence: firstNonEmpty(stringField(e, "confidence"), "INFERRED"),
			Weight:     numberField(e, "weight"),
			Props:      e,
		})
	}
	if reportPath != "" {
		if report, err := os.ReadFile(reportPath); err == nil {
			snap.ReportTitle = filepath.Base(reportPath)
			snap.ReportBody = string(report)
		}
	}
	return snap, nil
}

func graphifyNodeKind(n map[string]any) NodeKind {
	raw := strings.ToLower(firstNonEmpty(stringField(n, "kind"), stringField(n, "type")))
	switch raw {
	case "file":
		return NodeFile
	case "folder", "directory":
		return NodeFolder
	case "function":
		return NodeFunction
	case "class":
		return NodeClass
	case "method":
		return NodeMethod
	case "route":
		return NodeRoute
	case "tool":
		return NodeTool
	case "community":
		return NodeCommunity
	default:
		return NodeConcept
	}
}

func graphifyRelation(e map[string]any) Relation {
	raw := strings.ToLower(firstNonEmpty(stringField(e, "relation"), stringField(e, "type")))
	switch raw {
	case "contains":
		return RelContains
	case "defines":
		return RelDefines
	case "calls", "call":
		return RelCalls
	case "imports", "import":
		return RelImports
	case "extends":
		return RelExtends
	case "implements":
		return RelImplements
	case "member_of", "memberof":
		return RelMemberOf
	default:
		return RelRelated
	}
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func numberField(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
