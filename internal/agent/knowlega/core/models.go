package core

import (
	"context"
	"time"
)

// SourceManifestPipelineVersion is bumped when unchanged sources must be
// recompiled to pick up a materially different LLM Wiki generation contract.
// It is stored per source so a partially migrated corpus remains resumable.
const SourceManifestPipelineVersion = 4

type Confidence string

const (
	ConfidenceExtracted Confidence = "EXTRACTED"
	ConfidenceInferred  Confidence = "INFERRED"
	ConfidenceAmbiguous Confidence = "AMBIGUOUS"
)

type Project struct {
	ID        string
	Name      string
	RootPath  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ScopeBinding maps an external collaboration scope (for example a QM
// project) to a Knowledge Core project. The root path is server-owned routing
// metadata; callers must never be allowed to choose it per request.
type ScopeBinding struct {
	Provider        string
	ExternalScopeID string
	Kind            string
	OrganizationID  string
	ProjectID       string
	ProjectName     string
	RootPath        string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Source struct {
	ID           string
	ProjectID    string
	Path         string
	Kind         string
	Title        string
	SHA256       string
	Immutable    bool
	ImportedAt   time.Time
	OriginalPath string
}

type WikiPage struct {
	ID          string
	ProjectID   string
	Path        string
	Type        string
	Title       string
	Body        string
	Frontmatter map[string]any
	Sources     []string
	UpdatedAt   time.Time
}

type WikiPageEmbeddingStatus struct {
	Model        string
	SourceSHA256 string
}

type WikiPageEmbeddingMetadata struct {
	Model        string
	SourceSHA256 string
}

type WikiPageStore interface {
	UpsertWikiPages(context.Context, []WikiPage) error
	DeleteWikiPagesNotIn(context.Context, string, []string) error
	WikiPageEmbeddingStatus(context.Context, string, string) (WikiPageEmbeddingStatus, error)
	UpsertWikiPageEmbedding(context.Context, string, string, []float32, WikiPageEmbeddingMetadata) error
}

type WikiPageVersion struct {
	ID          string
	ProjectID   string
	PageID      string
	Path        string
	Body        string
	Frontmatter map[string]any
	Sources     []string
	Reason      string
	CreatedAt   time.Time
}

type SourceManifestEntry struct {
	ID                       string
	ProjectID                string
	QMFileID                 string
	QMProjectID              string
	QMScopeID                string
	QMSourceSHA256           string
	OriginalPath             string
	PipelineVersion          int
	SHA256                   string
	RawPath                  string
	ArchivePath              string
	OriginalRawPath          string
	ContentPath              string
	OriginalSHA256           string
	ContentSHA256            string
	Title                    string
	Files                    []string
	GenerationContractSHA256 string
	NewPageBudget            int
	NewPageCount             int
	CreatedPages             []string
	ReviewCount              int
	UpdatedAt                time.Time
}

type CodeRepo struct {
	ID                 string
	ProjectID          string
	RepoPath           string
	HeadCommit         string
	IndexedCommit      string
	PrimaryGraphSource string
	Stale              bool
	SyncedAt           *time.Time
}

type CodeGraphStore interface {
	UpsertCodeRepo(context.Context, CodeRepo) error
	DeleteGraphFacts(context.Context, string, string) error
	AddGraphNode(context.Context, GraphNode) error
	AddGraphEdge(context.Context, GraphEdge) error
}

type GraphNode struct {
	ID        string
	ProjectID string
	RepoID    string
	Domain    string
	ScopeID   string
	Kind      string
	Label     string
	SourceRef string
	Props     map[string]any
}

type GraphEdge struct {
	ID              string
	ProjectID       string
	RepoID          string
	Domain          string
	ScopeID         string
	SourceID        string
	TargetID        string
	Relation        string
	Confidence      Confidence
	ConfidenceScore float64
	Weight          float64
	Evidence        []string
	Props           map[string]any
}

type GraphEvidence struct {
	Path    string
	Title   string
	Content string
	Score   int
}

type ReviewItem struct {
	ID             string
	ProjectID      string
	Type           string
	Title          string
	Description    string
	Severity       string
	Status         string
	SourcePath     string
	SourcePaths    []string
	AffectedPages  []string
	SearchQueries  []string
	Options        []ReviewOption
	ResolvedAction string
	CreatedAt      time.Time
	ResolvedAt     *time.Time
}

type ReviewOption struct {
	Label  string
	Action string
}

type KnowledgeSearchResult struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Score   int    `json:"score"`
	Kind    string `json:"kind"`
}

type KnowledgeRequirement struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Kind string `json:"kind,omitempty"`
}

type KnowledgeEvidenceCheck struct {
	RequirementID string   `json:"requirement_id"`
	Status        string   `json:"status"`
	EvidencePaths []string `json:"evidence_paths,omitempty"`
	Explanation   string   `json:"explanation,omitempty"`
}

type KnowledgeCandidate struct {
	Path                   string   `json:"path"`
	Title                  string   `json:"title"`
	Kind                   string   `json:"kind"`
	Score                  int      `json:"score"`
	RecallRequirementIDs   []string `json:"recall_requirement_ids,omitempty"`
	RecallRequirementCount int      `json:"recall_requirement_count,omitempty"`
}

type KnowledgeCitation struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Aliases []string `json:"aliases,omitempty"`
}

type KnowledgeActionRecord struct {
	Action          string   `json:"action"`
	Query           string   `json:"query,omitempty"`
	Path            string   `json:"path,omitempty"`
	Candidate       string   `json:"candidate,omitempty"`
	RequirementID   string   `json:"requirement_id,omitempty"`
	WorkspaceStatus string   `json:"workspace_status,omitempty"`
	ResultCount     int      `json:"result_count,omitempty"`
	Sequence        int      `json:"sequence,omitempty"`
	ResultPaths     []string `json:"result_paths,omitempty"`
}

type KnowledgeValidationIssue struct {
	Code          string                 `json:"code"`
	RequirementID string                 `json:"requirement_id,omitempty"`
	Message       string                 `json:"message"`
	Repair        *KnowledgeRepairAction `json:"repair,omitempty"`
}

// KnowledgeRepairAction describes the next deterministic tool call needed to
// fix a validation protocol error.
type KnowledgeRepairAction struct {
	Action        string `json:"action"`
	Query         string `json:"query,omitempty"`
	Path          string `json:"path,omitempty"`
	Candidate     string `json:"candidate,omitempty"`
	RequirementID string `json:"requirement_id,omitempty"`
}

type KnowledgeSubmission struct {
	Question                 string                     `json:"question"`
	Answer                   string                     `json:"answer"`
	Status                   string                     `json:"status"`
	Candidate                string                     `json:"candidate,omitempty"`
	Requirements             []KnowledgeRequirement     `json:"requirements,omitempty"`
	Checks                   []KnowledgeEvidenceCheck   `json:"evidence_checks,omitempty"`
	EvidencePaths            []string                   `json:"evidence_paths,omitempty"`
	UnresolvedRequirementIDs []string                   `json:"unresolved_requirement_ids,omitempty"`
	Citations                []KnowledgeCitation        `json:"citations,omitempty"`
	ValidationIssues         []KnowledgeValidationIssue `json:"validation_issues,omitempty"`
}
