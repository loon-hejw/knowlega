package codegraph

import "time"

type NodeKind string

const (
	NodeFile      NodeKind = "File"
	NodeFolder    NodeKind = "Folder"
	NodePackage   NodeKind = "Package"
	NodeFunction  NodeKind = "Function"
	NodeClass     NodeKind = "Class"
	NodeMethod    NodeKind = "Method"
	NodeStruct    NodeKind = "Struct"
	NodeInterface NodeKind = "Interface"
	NodeType      NodeKind = "Type"
	NodeConst     NodeKind = "Const"
	NodeVariable  NodeKind = "Variable"
	NodeRoute     NodeKind = "Route"
	NodeTool      NodeKind = "Tool"
	NodeProcess   NodeKind = "Process"
	NodeCommunity NodeKind = "Community"
	NodeWikiPage  NodeKind = "WikiPage"
	NodeRawSource NodeKind = "RawSource"
	NodeConcept   NodeKind = "Concept"
)

type Relation string

const (
	RelContains      Relation = "CONTAINS"
	RelDefines       Relation = "DEFINES"
	RelCalls         Relation = "CALLS"
	RelImports       Relation = "IMPORTS"
	RelExtends       Relation = "EXTENDS"
	RelImplements    Relation = "IMPLEMENTS"
	RelEmbeds        Relation = "EMBEDS"
	RelHasMethod     Relation = "HAS_METHOD"
	RelHandlesRoute  Relation = "HANDLES_ROUTE"
	RelHandlesTool   Relation = "HANDLES_TOOL"
	RelMemberOf      Relation = "MEMBER_OF"
	RelStepInProcess Relation = "STEP_IN_PROCESS"
	RelWikiLink      Relation = "WIKILINK"
	RelDerivedFrom   Relation = "DERIVED_FROM"
	RelDocuments     Relation = "DOCUMENTS"
	RelReferences    Relation = "REFERENCES"
	RelCites         Relation = "CITES"
	RelSupports      Relation = "SUPPORTS"
	RelContradicts   Relation = "CONTRADICTS"
	RelRelatedTo     Relation = "RELATED_TO"
	RelRelated       Relation = "RELATED"
)

type Snapshot struct {
	Version      int         `json:"version"`
	RepoID       string      `json:"repo_id"`
	RepoPath     string      `json:"repo_path"`
	Commit       string      `json:"commit,omitempty"`
	Dirty        bool        `json:"dirty,omitempty"`
	SourceSHA256 string      `json:"source_sha256,omitempty"`
	Source       string      `json:"source"`
	ImportedAt   time.Time   `json:"imported_at"`
	Nodes        []Node      `json:"nodes"`
	Edges        []Edge      `json:"edges"`
	Communities  []Community `json:"communities,omitempty"`
	Processes    []Process   `json:"processes,omitempty"`
	Insights     Insights    `json:"insights,omitempty"`
	ReportTitle  string      `json:"report_title,omitempty"`
	ReportBody   string      `json:"report_body,omitempty"`
}

type Node struct {
	ID         string         `json:"id"`
	Kind       NodeKind       `json:"kind"`
	Label      string         `json:"label"`
	SourceFile string         `json:"source_file,omitempty"`
	Community  string         `json:"community,omitempty"`
	Props      map[string]any `json:"props,omitempty"`
}

type Edge struct {
	Source          string         `json:"source"`
	Target          string         `json:"target"`
	Relation        Relation       `json:"relation"`
	Confidence      string         `json:"confidence"`
	ConfidenceScore float64        `json:"confidence_score"`
	Weight          float64        `json:"weight"`
	Evidence        []string       `json:"evidence,omitempty"`
	Props           map[string]any `json:"props,omitempty"`
}

type Community struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Members  []string `json:"members"`
	Cohesion float64  `json:"cohesion"`
	TopKinds []string `json:"top_kinds,omitempty"`
}

type Process struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	EntryID string   `json:"entry_id"`
	Steps   []string `json:"steps"`
}

type InsightItem struct {
	NodeID string  `json:"node_id,omitempty"`
	EdgeID string  `json:"edge_id,omitempty"`
	Label  string  `json:"label"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score,omitempty"`
}

type Insights struct {
	GodNodes              []InsightItem `json:"god_nodes,omitempty"`
	SurprisingConnections []InsightItem `json:"surprising_connections,omitempty"`
	SuggestedQuestions    []string      `json:"suggested_questions,omitempty"`
}

type Adapter interface {
	Name() string
	Sync(repoPath string) (Snapshot, error)
	Query(repoID, query string, limit int) ([]Node, error)
	Impact(repoID, symbol string, depth int) (Snapshot, error)
}
