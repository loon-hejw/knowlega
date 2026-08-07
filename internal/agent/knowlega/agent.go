// Package knowlega exposes the LLM Wiki as an internal QM Agent.
//
// The Agent is deliberately a small orchestration boundary. Markdown under a
// scope workspace remains the durable source of truth; queue, compiler,
// query, review, and cleanup behavior are implemented by the service package.
package knowlega

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
)

type ScopeRef struct {
	OrgID           string
	ExternalScopeID string
	Kind            string
	Name            string
}

type ScopeStatus struct {
	Scope         ScopeRef
	ProjectID     string
	ProjectPath   string
	WikiPageCount int64
	SourceCount   int64
	Ready         bool
}

type QueryResult struct {
	ID     string
	Answer *core.QueryAnswer
}

type IngestResult struct {
	Task             service.IngestTask
	RawPath          string
	SkippedUnchanged bool
	GeneratedPaths   []string
	Reviews          []string
}

type AgentOptions struct {
	RootDir       string
	QueryAgent    service.QueryAgent
	Compiler      compiler.Provider
	ReviewAgent   service.WikiReviewAgent
	WikiStore     service.WikiPageStore
	SearchStore   service.SearchEvidenceStore
	GraphStore    service.GraphEvidenceStore
	QueryLogStore service.QueryLogStore
	Embedding     service.EmbeddingProvider
	AgentName     string
	QueryRuntime  service.QueryRuntimeOptions
	ProjectIDFor  func(ScopeRef) string
	ReadLimitByte int64
}

type Agent struct {
	rootDir      string
	queryAgent   service.QueryAgent
	compiler     compiler.Provider
	reviewAgent  service.WikiReviewAgent
	wikiStore    service.WikiPageStore
	searchStore  service.SearchEvidenceStore
	graphStore   service.GraphEvidenceStore
	queryLog     service.QueryLogStore
	embedding    service.EmbeddingProvider
	agentName    string
	queryRuntime service.QueryRuntimeOptions
	projectIDFor func(ScopeRef) string
	readLimit    int64

	queryMu sync.RWMutex
	queries map[string]*core.QueryAnswer
}

func New(opts AgentOptions) (*Agent, error) {
	root := strings.TrimSpace(opts.RootDir)
	if root == "" {
		return nil, errors.New("knowledge agent root directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if opts.Compiler == nil {
		opts.Compiler = compiler.MockProvider{}
	}
	if opts.QueryAgent == nil {
		opts.QueryAgent = service.MockQueryAgent{}
	}
	if opts.AgentName == "" {
		opts.AgentName = "mock"
	}
	if opts.ReadLimitByte <= 0 {
		opts.ReadLimitByte = 32 << 20
	}
	if opts.QueryRuntime.MaxSteps == 0 {
		opts.QueryRuntime = service.DefaultQueryRuntimeOptions()
	}
	return &Agent{
		rootDir:      abs,
		queryAgent:   opts.QueryAgent,
		compiler:     opts.Compiler,
		reviewAgent:  opts.ReviewAgent,
		wikiStore:    opts.WikiStore,
		searchStore:  opts.SearchStore,
		graphStore:   opts.GraphStore,
		queryLog:     opts.QueryLogStore,
		embedding:    opts.Embedding,
		agentName:    opts.AgentName,
		queryRuntime: opts.QueryRuntime,
		projectIDFor: opts.ProjectIDFor,
		readLimit:    opts.ReadLimitByte,
		queries:      make(map[string]*core.QueryAnswer),
	}, nil
}

func (a *Agent) EnsureScope(ctx context.Context, ref ScopeRef) (ScopeStatus, error) {
	if err := validateScope(ref); err != nil {
		return ScopeStatus{}, err
	}
	projectID := a.projectID(ref)
	projectPath := a.scopePath(projectID)
	if err := wiki.InitProject(wiki.ProjectOptions{Path: projectPath, Name: scopeName(ref)}); err != nil {
		return ScopeStatus{}, err
	}
	if err := os.MkdirAll(filepath.Join(projectPath, ".kbcore", "page-versions"), 0o755); err != nil {
		return ScopeStatus{}, err
	}
	if store, ok := a.wikiStore.(interface {
		UpsertProject(context.Context, core.Project) error
		UpsertScopeBinding(context.Context, core.ScopeBinding) error
	}); ok {
		now := time.Now().UTC()
		if err := store.UpsertProject(ctx, core.Project{ID: projectID, Name: scopeName(ref), RootPath: projectPath, CreatedAt: now, UpdatedAt: now}); err != nil {
			return ScopeStatus{}, err
		}
		if err := store.UpsertScopeBinding(ctx, core.ScopeBinding{Provider: "qm", ExternalScopeID: ref.ExternalScopeID, Kind: ref.Kind, OrganizationID: ref.OrgID, ProjectID: projectID, ProjectName: scopeName(ref), RootPath: projectPath, Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
			return ScopeStatus{}, err
		}
	}
	return a.Status(ref)
}

func (a *Agent) Status(ref ScopeRef) (ScopeStatus, error) {
	if err := validateScope(ref); err != nil {
		return ScopeStatus{}, err
	}
	projectID := a.projectID(ref)
	projectPath := a.scopePath(projectID)
	if _, err := os.Stat(filepath.Join(projectPath, "purpose.md")); err != nil {
		return ScopeStatus{}, fmt.Errorf("knowledge scope not found: %w", err)
	}
	pages, err := countMarkdown(filepath.Join(projectPath, "wiki"))
	if err != nil {
		return ScopeStatus{}, err
	}
	sources, err := countFiles(filepath.Join(projectPath, "raw", "sources"))
	if err != nil {
		return ScopeStatus{}, err
	}
	return ScopeStatus{Scope: ref, ProjectID: projectID, ProjectPath: projectPath, WikiPageCount: pages, SourceCount: sources, Ready: true}, nil
}

func (a *Agent) EnqueueFile(ref ScopeRef, sourcePath string) (service.IngestTask, error) {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return service.IngestTask{}, err
	}
	return a.enqueueBytes(ref, filepath.Base(sourcePath), "file", data)
}

// EnqueueBytes is used by QM persistence hooks when the source is already in
// the shared file/memory/session store and therefore has no filesystem path.
// It writes an immutable, content-addressed raw artifact before queueing it.
func (a *Agent) EnqueueBytes(ref ScopeRef, name, kind string, content []byte) (service.IngestTask, error) {
	return a.enqueueBytes(ref, name, kind, content)
}

func (a *Agent) EnqueueMemory(ref ScopeRef, revisionID string, content []byte) (service.IngestTask, error) {
	name := "memory-" + strings.TrimSpace(revisionID)
	if strings.TrimSuffix(name, "-") == "memory" {
		name = "memory"
	}
	return a.enqueueBytes(ref, name+".md", "memory", content)
}

func (a *Agent) EnqueueConversation(ref ScopeRef, conversationID string, content []byte, valuable bool) (service.IngestTask, error) {
	if !valuable {
		return service.IngestTask{}, nil
	}
	name := "conversation-" + strings.TrimSpace(conversationID)
	return a.enqueueBytes(ref, name+".md", "conversation", content)
}

func (a *Agent) Ingest(ctx context.Context, ref ScopeRef, sourceName string, content []byte) (IngestResult, error) {
	if len(content) == 0 {
		return IngestResult{}, errors.New("source content is required")
	}
	if _, err := a.EnsureScope(ctx, ref); err != nil {
		return IngestResult{}, err
	}
	task, err := a.enqueueBytes(ref, sourceName, "source", content)
	if err != nil {
		return IngestResult{}, err
	}
	queueResult, err := service.RunIngestQueue(service.RunIngestQueueOptions{
		ProjectPath: a.scopePath(a.projectID(ref)),
		MaxTasks:    1,
		KeepDone:    true,
		Validator:   a.queueValidator(),
	})
	if err != nil {
		return IngestResult{}, err
	}
	for _, processed := range queueResult.Tasks {
		if processed.ID != task.ID {
			continue
		}
		return IngestResult{Task: processed, RawPath: processed.RawPath, SkippedUnchanged: queueResult.Skipped > 0, GeneratedPaths: processed.Files}, nil
	}
	return IngestResult{Task: task, SkippedUnchanged: task.Status == service.IngestTaskDone && len(task.Files) == 0}, nil
}

func (a *Agent) Query(ctx context.Context, ref ScopeRef, question, conversationContext string, limit int) (QueryResult, error) {
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return QueryResult{}, err
	}
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		Context:             ctx,
		ProjectPath:         status.ProjectPath,
		ProjectID:           status.ProjectID,
		Question:            question,
		ConversationContext: conversationContext,
		Limit:               limit,
		Agent:               a.queryAgent,
		SearchStore:         a.searchStore,
		GraphStore:          a.graphStore,
		QueryLogStore:       a.queryLog,
		EmbeddingProvider:   a.embedding,
		Runtime:             a.queryRuntime,
	})
	if err != nil {
		return QueryResult{}, err
	}
	id := queryID(ref, answer)
	a.queryMu.Lock()
	a.queries[id] = answer
	a.queryMu.Unlock()
	if payloadStore, ok := a.queryLog.(service.QueryAnswerStore); ok {
		payload, marshalErr := json.Marshal(answer)
		if marshalErr != nil {
			return QueryResult{}, marshalErr
		}
		if err := payloadStore.SaveQueryAnswerPayload(ctx, id, status.ProjectID, payload); err != nil {
			return QueryResult{}, err
		}
	}
	return QueryResult{ID: id, Answer: answer}, nil
}

func (a *Agent) SaveQueryAnswer(ctx context.Context, ref ScopeRef, queryID, title string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.queryMu.RLock()
	answer := a.queries[queryID]
	a.queryMu.RUnlock()
	status, err := a.Status(ref)
	if err != nil {
		return "", err
	}
	if answer == nil {
		if payloadStore, ok := a.queryLog.(service.QueryAnswerStore); ok {
			payload, loadErr := payloadStore.LoadQueryAnswerPayload(ctx, queryID, status.ProjectID)
			if loadErr != nil {
				return "", loadErr
			}
			if len(payload) > 0 {
				var restored core.QueryAnswer
				if unmarshalErr := json.Unmarshal(payload, &restored); unmarshalErr != nil {
					return "", unmarshalErr
				}
				answer = &restored
			}
		}
	}
	if answer == nil {
		return "", fmt.Errorf("query answer not found: %s", queryID)
	}
	result, err := service.WriteQueryAnswer(service.QueryWritebackOptions{ProjectPath: status.ProjectPath, Title: title, Answer: answer})
	if err != nil {
		return "", err
	}
	return result.Path, nil
}

func (a *Agent) Maintain(ctx context.Context, ref ScopeRef, runLLMReview bool) (service.MaintainWikiResult, error) {
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return service.MaintainWikiResult{}, err
	}
	opts := service.MaintainWikiOptions{
		ProjectPath:       status.ProjectPath,
		ProjectID:         status.ProjectID,
		QueueValidator:    a.queueValidator(),
		SkipUnchanged:     true,
		RetryFailed:       true,
		KeepDone:          true,
		RunLLMReview:      runLLMReview,
		ReviewAgent:       a.reviewAgent,
		SweepAgent:        a.queryAgent,
		RunPGSync:         a.wikiStore != nil,
		WikiStore:         a.wikiStore,
		EmbeddingProvider: a.embedding,
		Context:           ctx,
	}
	return service.MaintainWiki(opts)
}

func (a *Agent) Cleanup(ref ScopeRef, sourcePath string, deleteRaw bool) (service.DeleteSourceResult, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.DeleteSourceResult{}, err
	}
	return service.DeleteSource(service.DeleteSourceOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, SourcePath: sourcePath, DeleteRaw: deleteRaw})
}

func (a *Agent) queueValidator() service.IngestQueueValidator {
	return func(opts service.QueueValidateOptions) (service.QueueValidateResult, error) {
		result, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
			ProjectPath:   opts.ProjectPath,
			SourcePath:    opts.SourcePath,
			Title:         opts.Title,
			Provider:      a.compiler,
			SkipUnchanged: opts.SkipUnchanged,
		})
		if err != nil {
			return service.QueueValidateResult{}, err
		}
		if len(result.Results) == 0 {
			return service.QueueValidateResult{}, nil
		}
		last := result.Results[len(result.Results)-1]
		return service.QueueValidateResult{RawPath: last.RawPath, Files: last.Files, Skipped: last.Skipped, SHA256: last.SHA256}, nil
	}
}

func (a *Agent) enqueueBytes(ref ScopeRef, name, kind string, content []byte) (service.IngestTask, error) {
	if err := validateScope(ref); err != nil {
		return service.IngestTask{}, err
	}
	if len(content) == 0 {
		return service.IngestTask{}, errors.New("source content is required")
	}
	status, err := a.EnsureScope(context.Background(), ref)
	if err != nil {
		return service.IngestTask{}, err
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "source.md"
	}
	base = strings.ReplaceAll(base, "..", "_")
	rel := filepath.ToSlash(filepath.Join("raw", "sources", hash[:16]+"-"+base))
	abs := filepath.Join(status.ProjectPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return service.IngestTask{}, err
	}
	if existing, readErr := os.ReadFile(abs); readErr == nil {
		if string(existing) != string(content) {
			return service.IngestTask{}, fmt.Errorf("immutable source collision at %s", rel)
		}
	} else if errors.Is(readErr, os.ErrNotExist) {
		if err := os.WriteFile(abs, content, 0o644); err != nil {
			return service.IngestTask{}, err
		}
	} else {
		return service.IngestTask{}, readErr
	}
	return service.QueueIngestSource(service.QueueIngestOptions{ProjectPath: status.ProjectPath, SourcePath: abs, Title: sourceTitle(kind, name)})
}

func (a *Agent) projectID(ref ScopeRef) string {
	if a.projectIDFor != nil {
		if id := strings.TrimSpace(a.projectIDFor(ref)); id != "" {
			return id
		}
	}
	return core.StableID("qm-knowlega", ref.OrgID, ref.Kind, ref.ExternalScopeID)
}

func (a *Agent) scopePath(projectID string) string {
	return filepath.Join(a.rootDir, "scopes", projectID)
}

func validateScope(ref ScopeRef) error {
	if strings.TrimSpace(ref.OrgID) == "" || strings.TrimSpace(ref.ExternalScopeID) == "" || strings.TrimSpace(ref.Kind) == "" {
		return errors.New("org_id, external_scope_id, and scope kind are required")
	}
	return nil
}

func scopeName(ref ScopeRef) string {
	if strings.TrimSpace(ref.Name) != "" {
		return strings.TrimSpace(ref.Name)
	}
	return ref.Kind + ":" + ref.ExternalScopeID
}

func sourceTitle(kind, name string) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSpace(name)), filepath.Ext(strings.TrimSpace(name)))
	if base == "" {
		base = kind
	}
	return base
}

func queryID(ref ScopeRef, answer *core.QueryAnswer) string {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	return core.StableID("qm-query", ref.OrgID, ref.Kind, ref.ExternalScopeID, answer.Question, answer.Answer, stamp)
}

func countMarkdown(root string) (int64, error) {
	var count int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			count++
		}
		return nil
	})
	return count, err
}

func countFiles(root string) (int64, error) {
	var count int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}
