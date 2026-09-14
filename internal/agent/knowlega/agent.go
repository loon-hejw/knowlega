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
	Scope            ScopeRef
	ProjectID        string
	ProjectPath      string
	State            string
	WikiPageCount    int64
	SourceCount      int64
	Queue            service.WorkspaceQueueStatus
	LastError        string
	LastSuccessfulAt *time.Time
	Ready            bool
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
	Compiler      compiler.Provider
	ReviewAgent   service.WikiReviewAgent
	Maintenance   service.MaintenanceSynthesisAgent
	WikiStore     service.WikiPageStore
	SearchStore   service.SearchEvidenceStore
	GraphStore    service.GraphEvidenceStore
	Embedding     service.EmbeddingProvider
	AgentName     string
	ProjectIDFor  func(ScopeRef) string
	ReadLimitByte int64
}

type Agent struct {
	rootDir      string
	compiler     compiler.Provider
	reviewAgent  service.WikiReviewAgent
	maintenance  service.MaintenanceSynthesisAgent
	wikiStore    service.WikiPageStore
	searchStore  service.SearchEvidenceStore
	graphStore   service.GraphEvidenceStore
	embedding    service.EmbeddingProvider
	agentName    string
	projectIDFor func(ScopeRef) string
	readLimit    int64
	maintainMu   sync.Map
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
	if opts.AgentName == "" {
		opts.AgentName = "mock"
	}
	if opts.ReadLimitByte <= 0 {
		opts.ReadLimitByte = 32 << 20
	}
	return &Agent{
		rootDir:      abs,
		compiler:     opts.Compiler,
		reviewAgent:  opts.ReviewAgent,
		maintenance:  opts.Maintenance,
		wikiStore:    opts.WikiStore,
		searchStore:  opts.SearchStore,
		graphStore:   opts.GraphStore,
		embedding:    opts.Embedding,
		agentName:    opts.AgentName,
		projectIDFor: opts.ProjectIDFor,
		readLimit:    opts.ReadLimitByte,
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
	return a.RefreshScope(ctx, ref)
}

// RefreshScope derives the current filesystem state and mirrors it into the
// optional PostgreSQL scope binding. Markdown and .kbcore remain authoritative.
func (a *Agent) RefreshScope(ctx context.Context, ref ScopeRef) (ScopeStatus, error) {
	status, err := a.Status(ref)
	if err != nil {
		return ScopeStatus{}, err
	}
	if store, ok := a.wikiStore.(interface {
		UpsertProject(context.Context, core.Project) error
		UpsertScopeBinding(context.Context, core.ScopeBinding) error
	}); ok {
		now := time.Now().UTC()
		if err := store.UpsertProject(ctx, core.Project{ID: status.ProjectID, Name: scopeName(ref), RootPath: status.ProjectPath, CreatedAt: now, UpdatedAt: now}); err != nil {
			return ScopeStatus{}, err
		}
		if err := store.UpsertScopeBinding(ctx, core.ScopeBinding{Provider: "qm", ExternalScopeID: ref.ExternalScopeID, Kind: ref.Kind, OrganizationID: ref.OrgID, ProjectID: status.ProjectID, ProjectName: scopeName(ref), RootPath: status.ProjectPath, Status: status.State, CreatedAt: now, UpdatedAt: now}); err != nil {
			return ScopeStatus{}, err
		}
	}
	return status, nil
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
	workspaceState, err := service.InspectKnowledgeWorkspace(projectPath)
	if err != nil {
		return ScopeStatus{}, err
	}
	return ScopeStatus{
		Scope:            ref,
		ProjectID:        projectID,
		ProjectPath:      projectPath,
		State:            workspaceState.Status,
		WikiPageCount:    workspaceState.WikiPageCount,
		SourceCount:      workspaceState.SourceCount,
		Queue:            workspaceState.Queue,
		LastError:        workspaceState.LastError,
		LastSuccessfulAt: workspaceState.LastSuccessfulAt,
		Ready:            workspaceState.Status == service.KnowledgeWorkspaceReady,
	}, nil
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

func (a *Agent) EnqueueQMFile(ref ScopeRef, fileID, name, expectedSHA256 string, content []byte) (service.IngestTask, error) {
	if err := validateScope(ref); err != nil {
		return service.IngestTask{}, err
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" || strings.ContainsAny(fileID, `/\\`) {
		return service.IngestTask{}, errors.New("qm file id is required")
	}
	if len(content) == 0 {
		return service.IngestTask{}, errors.New("source content is required")
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	if expectedSHA256 != "" && !strings.EqualFold(expectedSHA256, hash) {
		return service.IngestTask{}, errors.New("qm file sha256 does not match content")
	}
	status, err := a.EnsureScope(context.Background(), ref)
	if err != nil {
		return service.IngestTask{}, err
	}
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "source.md"
	}
	base = strings.ReplaceAll(base, "..", "_")
	rel := filepath.ToSlash(filepath.Join("raw", "sources", "qm", fileID, hash[:16]+"-"+base))
	abs := filepath.Join(status.ProjectPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return service.IngestTask{}, err
	}
	if existing, readErr := os.ReadFile(abs); readErr == nil {
		if string(existing) != string(content) {
			return service.IngestTask{}, fmt.Errorf("immutable source collision at %s", rel)
		}
	} else if errors.Is(readErr, os.ErrNotExist) {
		if err := os.WriteFile(abs, content, 0o444); err != nil {
			return service.IngestTask{}, err
		}
	} else {
		return service.IngestTask{}, readErr
	}
	return service.QueueIngestSource(service.QueueIngestOptions{ProjectPath: status.ProjectPath, SourcePath: abs, Title: sourceTitle("file", name)})
}

func (a *Agent) IngestTask(ref ScopeRef, taskID string) (service.IngestTask, bool, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.IngestTask{}, false, err
	}
	queue, err := service.LoadIngestQueue(status.ProjectPath)
	if err != nil {
		return service.IngestTask{}, false, err
	}
	for _, task := range queue.Tasks {
		if task.ID == taskID {
			return task, true, nil
		}
	}
	return service.IngestTask{}, false, nil
}

func (a *Agent) BindQMFile(ref ScopeRef, sourcePath, fileID, qmProjectID, sha256 string) error {
	status, err := a.Status(ref)
	if err != nil {
		return err
	}
	return service.BindQMSource(status.ProjectPath, sourcePath, fileID, qmProjectID, ref.ExternalScopeID, sha256)
}

func (a *Agent) SyncSourceManifest(ctx context.Context, ref ScopeRef) error {
	if a.wikiStore == nil {
		return nil
	}
	status, err := a.Status(ref)
	if err != nil {
		return err
	}
	_, err = service.SyncSourceManifestToStore(ctx, service.WikiSyncOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, Store: a.wikiStore})
	return err
}

func (a *Agent) SyncWiki(ctx context.Context, ref ScopeRef) error {
	if a.wikiStore == nil {
		return nil
	}
	status, err := a.Status(ref)
	if err != nil {
		return err
	}
	_, err = service.SyncWikiPagesToStore(ctx, service.WikiSyncOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, Store: a.wikiStore, EmbeddingProvider: a.embedding})
	return err
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

func (a *Agent) Search(ctx context.Context, ref ScopeRef, query string, limit int) ([]core.KnowledgeSearchResult, error) {
	return a.SearchScoped(ctx, ref, query, limit, "all")
}

func (a *Agent) SearchScoped(ctx context.Context, ref ScopeRef, query string, limit int, scope string) ([]core.KnowledgeSearchResult, error) {
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return nil, err
	}
	return service.SearchProjectDocumentsScoped(ctx, status.ProjectPath, status.ProjectID, query, limit, scope, a.searchStore, a.embedding)
}

func (a *Agent) Read(ref ScopeRef, path string) (service.KnowledgeDocument, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.KnowledgeDocument{}, err
	}
	return service.ReadProjectDocument(status.ProjectPath, path)
}

func (a *Agent) List(ref ScopeRef, query string, limit int) ([]service.KnowledgePage, error) {
	status, err := a.Status(ref)
	if err != nil {
		return nil, err
	}
	return service.ListProjectPages(status.ProjectPath, query, limit)
}

func (a *Agent) FollowLinks(ref ScopeRef, path string, limit int) (service.KnowledgeFollowResult, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.KnowledgeFollowResult{}, err
	}
	return service.FollowProjectLinks(status.ProjectPath, path, limit)
}

func (a *Agent) Discover(ctx context.Context, ref ScopeRef, requirements []core.KnowledgeRequirement, limit int) ([]core.KnowledgeCandidate, error) {
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return nil, err
	}
	return service.DiscoverProjectCandidates(ctx, status.ProjectPath, status.ProjectID, requirements, limit, a.searchStore, a.embedding)
}

func (a *Agent) Graph(ctx context.Context, ref ScopeRef, query string, seedPaths []string, limit int) ([]service.KnowledgeDocument, error) {
	status, err := a.Status(ref)
	if err != nil {
		return nil, err
	}
	return service.SearchProjectGraph(ctx, status.ProjectPath, status.ProjectID, query, seedPaths, limit, a.graphStore)
}

func (a *Agent) Writeback(ctx context.Context, ref ScopeRef, title string, submission core.KnowledgeSubmission) (service.KnowledgeWritebackResult, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.KnowledgeWritebackResult{}, err
	}
	return service.WriteKnowledgeSubmission(service.KnowledgeWritebackOptions{
		Context: ctx, ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, Title: title, Submission: submission,
		WikiStore: a.wikiStore, EmbeddingProvider: a.embedding,
	})
}

func (a *Agent) Maintain(ctx context.Context, ref ScopeRef, runLLMReview bool) (service.MaintainWikiResult, error) {
	if err := validateScope(ref); err != nil {
		return service.MaintainWikiResult{}, err
	}
	lock := a.scopeMaintenanceLock(a.projectID(ref))
	lock.Lock()
	defer lock.Unlock()
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return service.MaintainWikiResult{}, err
	}
	opts := service.MaintainWikiOptions{
		ProjectPath:    status.ProjectPath,
		ProjectID:      status.ProjectID,
		QueueValidator: a.queueValidator(),
		SkipUnchanged:  true,
		RetryFailed:    true,
		KeepDone:       true,
		RunLLMReview:   runLLMReview,
		ReviewAgent:    a.reviewAgent,
		SweepAgent:     a.maintenance,
		RefreshOverview: func() (bool, error) {
			return compiler.RefreshOverview(a.compiler, status.ProjectPath)
		},
		RunPGSync:         a.wikiStore != nil,
		WikiStore:         a.wikiStore,
		EmbeddingProvider: a.embedding,
		Context:           ctx,
	}
	result, err := service.MaintainWiki(opts)
	if err != nil {
		return result, err
	}
	if result.Status == "failed" {
		for _, step := range result.Steps {
			if step.Status == "failed" {
				return result, fmt.Errorf("knowledge maintenance %s failed: %s", step.Name, step.Error)
			}
		}
		return result, errors.New("knowledge maintenance failed")
	}
	return result, nil
}

func (a *Agent) scopeMaintenanceLock(projectID string) *sync.Mutex {
	value, _ := a.maintainMu.LoadOrStore(projectID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (a *Agent) RecoverIngestQueue(ctx context.Context, ref ScopeRef) (int, error) {
	status, err := a.EnsureScope(ctx, ref)
	if err != nil {
		return 0, err
	}
	return service.RecoverProcessingIngestTasks(status.ProjectPath)
}

func (a *Agent) RecoverMaintenance(ref ScopeRef) (bool, error) {
	if err := validateScope(ref); err != nil {
		return false, err
	}
	return service.RecoverInterruptedKnowledgeMaintenance(a.scopePath(a.projectID(ref)))
}

func (a *Agent) Cleanup(ref ScopeRef, sourcePath string, deleteRaw bool) (service.DeleteSourceResult, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.DeleteSourceResult{}, err
	}
	return service.DeleteSource(service.DeleteSourceOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, SourcePath: sourcePath, DeleteRaw: deleteRaw})
}

func (a *Agent) RemoveQMFile(ref ScopeRef, sourcePath, taskID string) (service.DeleteSourceResult, error) {
	status, err := a.Status(ref)
	if err != nil {
		return service.DeleteSourceResult{}, err
	}
	result, err := service.DeleteSource(service.DeleteSourceOptions{ProjectPath: status.ProjectPath, ProjectID: status.ProjectID, SourcePath: sourcePath, DeleteRaw: true})
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, service.ErrSourceNotFound) {
		return service.DeleteSourceResult{}, err
	}
	abs := sourcePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(status.ProjectPath, filepath.FromSlash(sourcePath))
	}
	if err := service.RemoveQueuedSource(status.ProjectPath, taskID, abs); err != nil {
		return service.DeleteSourceResult{}, err
	}
	return service.DeleteSourceResult{RawPath: filepath.ToSlash(sourcePath), DeletedRaw: true, ManifestUpdated: false}, nil
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
