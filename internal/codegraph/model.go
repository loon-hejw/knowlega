package codegraph

import "time"

type NodeKind string

const (
	NodeFile      NodeKind = "File"
	NodeFolder    NodeKind = "Folder"
	NodeFunction  NodeKind = "Function"
	NodeClass     NodeKind = "Class"
	NodeMethod    NodeKind = "Method"
	NodeRoute     NodeKind = "Route"
	NodeTool      NodeKind = "Tool"
	NodeProcess   NodeKind = "Process"
	NodeCommunity NodeKind = "Community"
	NodeConcept   NodeKind = "Concept"
)

type Relation string

const (
	RelContains     Relation = "CONTAINS"
	RelDefines      Relation = "DEFINES"
	RelCalls        Relation = "CALLS"
	RelImports      Relation = "IMPORTS"
	RelExtends      Relation = "EXTENDS"
	RelImplements   Relation = "IMPLEMENTS"
	RelHandlesRoute Relation = "HANDLES_ROUTE"
	RelHandlesTool  Relation = "HANDLES_TOOL"
	RelMemberOf     Relation = "MEMBER_OF"
	RelRelated      Relation = "RELATED"
)

type Snapshot struct {
	RepoID      string
	RepoPath    string
	Commit      string
	Source      string
	ImportedAt  time.Time
	Nodes       []Node
	Edges       []Edge
	ReportTitle string
	ReportBody  string
}

type Node struct {
	ID         string
	Kind       NodeKind
	Label      string
	SourceFile string
	Community  string
	Props      map[string]any
}

type Edge struct {
	Source     string
	Target     string
	Relation   Relation
	Confidence string
	Weight     float64
	Props      map[string]any
}

type Adapter interface {
	Name() string
	Sync(repoPath string) (Snapshot, error)
	Query(repoID, query string, limit int) ([]Node, error)
	Impact(repoID, symbol string, depth int) (Snapshot, error)
}
