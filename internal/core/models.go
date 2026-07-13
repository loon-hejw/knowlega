package core

import (
	"context"
	"time"
)

// SourceManifestPipelineVersion is bumped when unchanged sources must be
// recompiled to pick up a materially different LLM Wiki generation contract.
// It is stored per source so a partially migrated corpus remains resumable.
const SourceManifestPipelineVersion = 2

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
	ID              string
	ProjectID       string
	OriginalPath    string
	PipelineVersion int
	SHA256          string
	RawPath         string
	ArchivePath     string
	OriginalRawPath string
	ContentPath     string
	OriginalSHA256  string
	ContentSHA256   string
	Title           string
	Files           []string
	ReviewCount     int
	UpdatedAt       time.Time
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

type QueryResult struct {
	Path    string
	Title   string
	Snippet string
	Score   int
	Kind    string
}

type QuerySearch struct {
	Text      string `json:"text"`
	Weight    int    `json:"weight"`
	Rationale string `json:"rationale"`
}

type QueryPlan struct {
	Question       string        `json:"question"`
	Intent         string        `json:"intent"`
	ReadFirst      []string      `json:"read_first"`
	Searches       []QuerySearch `json:"searches"`
	CandidateLimit int           `json:"candidate_limit"`
	AnswerMode     string        `json:"answer_mode"`
	CanWriteBack   bool          `json:"can_write_back"`
}

type QueryAction struct {
	Action    string `json:"action"`
	Path      string `json:"path,omitempty"`
	Query     string `json:"query,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Title     string `json:"title,omitempty"`
	Answer    string `json:"answer,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

type QueryTraceStep struct {
	Step        int         `json:"step"`
	Action      QueryAction `json:"action"`
	Observation string      `json:"observation"`
}

type QueryCitation struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

type QueryAnswer struct {
	Question                string           `json:"question"`
	Plan                    QueryPlan        `json:"plan"`
	Results                 []QueryResult    `json:"results"`
	Answer                  string           `json:"answer"`
	SuggestedWritebackTitle string           `json:"suggested_writeback_title,omitempty"`
	Citations               []QueryCitation  `json:"citations"`
	Trace                   []QueryTraceStep `json:"trace,omitempty"`
	Notes                   []string         `json:"notes"`
}
