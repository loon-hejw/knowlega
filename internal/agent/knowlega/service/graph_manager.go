package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

type GraphWebhookResult struct {
	Accepted bool          `json:"accepted"`
	Ignored  bool          `json:"ignored"`
	Reason   string        `json:"reason,omitempty"`
	Job      GraphIndexJob `json:"job,omitempty"`
}

type GraphRepositoryStatus struct {
	RegistryID   string `json:"registry_id"`
	Provider     string `json:"provider"`
	RepositoryID string `json:"repository_id"`
	FullName     string `json:"full_name"`
	Branch       string `json:"branch"`
	Disabled     bool   `json:"disabled"`
	Indexed      bool   `json:"indexed"`
	Commit       string `json:"commit,omitempty"`
	Dirty        bool   `json:"dirty,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
}

type GraphJobStatus string

const (
	GraphJobPending    GraphJobStatus = "pending"
	GraphJobRunning    GraphJobStatus = "running"
	GraphJobDone       GraphJobStatus = "done"
	GraphJobFailed     GraphJobStatus = "failed"
	GraphJobSuperseded GraphJobStatus = "superseded"
)

type GraphIndexJob struct {
	ID           string         `json:"id"`
	RegistryID   string         `json:"registry_id"`
	RepositoryID string         `json:"repository_id"`
	FullName     string         `json:"full_name"`
	Branch       string         `json:"branch"`
	Commit       string         `json:"commit,omitempty"`
	DeliveryID   string         `json:"delivery_id,omitempty"`
	Trigger      string         `json:"trigger"`
	Status       GraphJobStatus `json:"status"`
	Attempts     int            `json:"attempts"`
	Error        string         `json:"error,omitempty"`
	SnapshotPath string         `json:"snapshot_path,omitempty"`
	NodeCount    int            `json:"node_count,omitempty"`
	EdgeCount    int            `json:"edge_count,omitempty"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

type graphJobQueue struct {
	Version int             `json:"version"`
	Jobs    []GraphIndexJob `json:"jobs"`
}

type QueueGraphJobOptions struct {
	RegistryID   string `json:"registry_id"`
	RepositoryID string `json:"repository_id"`
	FullName     string `json:"full_name"`
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	DeliveryID   string `json:"delivery_id"`
	Trigger      string `json:"trigger"`
}

type GraphManagerOptions struct {
	ProjectPath       string
	ProjectID         string
	Config            config.GraphConfig
	Store             CodeGraphStore
	WikiStore         WikiPageStore
	EmbeddingProvider EmbeddingProvider
	SemanticEnricher  GraphSemanticEnricher
}

type GraphManager struct {
	projectPath       string
	projectID         string
	config            config.GraphConfig
	store             CodeGraphStore
	wikiStore         WikiPageStore
	embeddingProvider EmbeddingProvider
	semanticEnricher  GraphSemanticEnricher
	wake              chan struct{}
	mu                sync.Mutex
}

type managedRepoMarker struct {
	Version      int    `json:"version"`
	RegistryID   string `json:"registry_id"`
	RepositoryID string `json:"repository_id"`
	CloneURL     string `json:"clone_url"`
	CreatedAt    string `json:"created_at"`
}

func NewGraphManager(opts GraphManagerOptions) (*GraphManager, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	if opts.Config.MaxParallelJobs <= 0 {
		opts.Config.MaxParallelJobs = 2
	}
	manager := &GraphManager{
		projectPath: opts.ProjectPath, projectID: firstNonEmptyString(opts.ProjectID, "local"),
		config: opts.Config, store: opts.Store, wikiStore: opts.WikiStore, embeddingProvider: opts.EmbeddingProvider,
		semanticEnricher: opts.SemanticEnricher, wake: make(chan struct{}, 1),
	}
	if err := manager.recoverQueue(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *GraphManager) Queue(opts QueueGraphJobOptions) (GraphIndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(m.projectPath, "wiki", "index.md")); err != nil {
		return GraphIndexJob{}, fmt.Errorf("project is not ready for graph jobs: %w", err)
	}
	registry, repo, err := m.lookupRepository(opts.RegistryID, opts.RepositoryID, opts.FullName)
	if err != nil {
		return GraphIndexJob{}, err
	}
	branch := firstNonEmptyString(opts.Branch, repo.Branch)
	if branch != repo.Branch {
		return GraphIndexJob{}, fmt.Errorf("branch %q is not allowlisted for %s/%s", branch, registry.ID, repo.ID)
	}
	queue, err := m.loadQueueLocked()
	if err != nil {
		return GraphIndexJob{}, err
	}
	for _, existing := range queue.Jobs {
		if opts.DeliveryID != "" && existing.RegistryID == registry.ID && existing.DeliveryID == opts.DeliveryID {
			return existing, nil
		}
		if opts.Commit != "" && existing.RegistryID == registry.ID && existing.RepositoryID == repo.ID && existing.Commit == opts.Commit && existing.Status == GraphJobDone {
			return existing, nil
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for index := range queue.Jobs {
		job := &queue.Jobs[index]
		if job.RegistryID == registry.ID && job.RepositoryID == repo.ID && job.Status == GraphJobPending {
			job.Status = GraphJobSuperseded
			job.UpdatedAt = now
		}
	}
	job := GraphIndexJob{
		ID:         core.StableID("graph-job", registry.ID, repo.ID, opts.Commit, opts.DeliveryID, now, fmt.Sprint(len(queue.Jobs))),
		RegistryID: registry.ID, RepositoryID: repo.ID, FullName: repo.FullName,
		Branch: branch, Commit: strings.TrimSpace(opts.Commit), DeliveryID: strings.TrimSpace(opts.DeliveryID),
		Trigger: firstNonEmptyString(opts.Trigger, "manual"), Status: GraphJobPending, CreatedAt: now, UpdatedAt: now,
	}
	queue.Jobs = append(queue.Jobs, job)
	if err := m.saveQueueLocked(queue); err != nil {
		return GraphIndexJob{}, err
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return job, nil
}

func (m *GraphManager) HandleWebhook(registryID string, headers http.Header, body []byte) (GraphWebhookResult, error) {
	registry, _, err := m.lookupRegistry(registryID)
	if err != nil {
		return GraphWebhookResult{}, err
	}
	if len(body) == 0 || len(body) > 2<<20 {
		return GraphWebhookResult{}, fmt.Errorf("webhook payload must be between 1 byte and 2 MiB")
	}
	if err := verifyRegistryWebhook(registry, headers, body); err != nil {
		return GraphWebhookResult{}, err
	}
	fullName, branch, commit, delivery, event, err := parseRegistryPush(registry.Provider, headers, body)
	if err != nil {
		return GraphWebhookResult{}, err
	}
	if event != "push" {
		return GraphWebhookResult{Accepted: true, Ignored: true, Reason: "event is not a branch push"}, nil
	}
	if branch == "" || commit == "" || strings.Trim(commit, "0") == "" {
		return GraphWebhookResult{Accepted: true, Ignored: true, Reason: "deleted or non-branch ref"}, nil
	}
	if delivery == "" {
		delivery = graphDeliveryHash(body)
	}
	job, err := m.Queue(QueueGraphJobOptions{
		RegistryID: registry.ID, FullName: fullName, Branch: branch, Commit: commit,
		DeliveryID: delivery, Trigger: "webhook",
	})
	if err != nil {
		return GraphWebhookResult{}, err
	}
	return GraphWebhookResult{Accepted: true, Job: job}, nil
}

func (m *GraphManager) QueueInitialMissing() error {
	for _, registry := range m.config.Registries {
		for _, repo := range registry.Repositories {
			if repo.Disabled {
				continue
			}
			if _, ok := loadLatestCodeSnapshotPointer(m.projectPath, repo.ID); ok {
				continue
			}
			if _, err := m.Queue(QueueGraphJobOptions{RegistryID: registry.ID, RepositoryID: repo.ID, Branch: repo.Branch, Trigger: "initial"}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *GraphManager) List() ([]GraphIndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue, err := m.loadQueueLocked()
	if err != nil {
		return nil, err
	}
	out := append([]GraphIndexJob(nil), queue.Jobs...)
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, nil
}

func (m *GraphManager) Repositories() []GraphRepositoryStatus {
	var out []GraphRepositoryStatus
	for _, registry := range m.config.Registries {
		for _, repo := range registry.Repositories {
			status := GraphRepositoryStatus{
				RegistryID: registry.ID, Provider: registry.Provider, RepositoryID: repo.ID,
				FullName: repo.FullName, Branch: repo.Branch, Disabled: repo.Disabled,
			}
			if pointer, ok := loadLatestCodeSnapshotPointer(m.projectPath, repo.ID); ok {
				status.Indexed = true
				status.Commit = pointer.Commit
				status.Dirty = pointer.Dirty
				status.SourceSHA256 = pointer.SourceSHA256
				status.SnapshotPath = pointer.SnapshotPath
			}
			out = append(out, status)
		}
	}
	return out
}

func (m *GraphManager) Run(ctx context.Context) {
	_ = m.QueueInitialMissing()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		_, _ = m.RunPendingOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *GraphManager) RunPendingOnce(ctx context.Context) ([]GraphIndexJob, error) {
	jobs, err := m.claimPendingJobs()
	if err != nil || len(jobs) == 0 {
		return jobs, err
	}
	type outcome struct {
		job    GraphIndexJob
		result GoCodeIndexResult
		err    error
	}
	outcomes := make(chan outcome, len(jobs))
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := m.processJob(ctx, job)
			outcomes <- outcome{job: job, result: result, err: err}
		}()
	}
	wg.Wait()
	close(outcomes)
	var completed []GraphIndexJob
	for outcome := range outcomes {
		updated, updateErr := m.finishJob(outcome.job.ID, outcome.result, outcome.err)
		if updateErr != nil {
			return completed, updateErr
		}
		completed = append(completed, updated)
	}
	return completed, nil
}

func (m *GraphManager) processJob(ctx context.Context, job GraphIndexJob) (GoCodeIndexResult, error) {
	registry, repo, err := m.lookupRepository(job.RegistryID, job.RepositoryID, job.FullName)
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	checkout, err := m.ensureManagedCheckout(ctx, registry, repo, job.Commit)
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	result, err := IndexGoCodeSnapshot(GoCodeIndexOptions{ProjectPath: m.projectPath, RepoID: repo.ID, RepoPath: checkout})
	if err != nil {
		return GoCodeIndexResult{}, err
	}
	if m.store != nil {
		if _, err := SyncCodeGraphSnapshot(ctx, m.store, m.projectID, result.Snapshot, result.SnapshotPath); err != nil {
			return GoCodeIndexResult{}, err
		}
	}
	if m.wikiStore != nil {
		paths := append([]string{"wiki/index.md", "wiki/log.md", "wiki/overview.md"}, result.WrittenPaths...)
		if _, err := SyncWikiPagePathsToStore(ctx, WikiSyncOptions{
			ProjectPath: m.projectPath, ProjectID: m.projectID, Store: m.wikiStore,
			EmbeddingProvider: m.embeddingProvider,
		}, paths); err != nil {
			return GoCodeIndexResult{}, err
		}
	}
	if m.config.SemanticEnrichment && m.semanticEnricher != nil {
		if count, enrichErr := m.semanticEnricher.Enrich(ctx, m.projectPath); enrichErr != nil {
			_ = appendLog(m.projectPath, "graph-semantic-enrichment", repo.ID, "Semantic enrichment failed after exact indexing: "+redactGraphError(enrichErr.Error(), m.config))
		} else if count > 0 {
			_ = appendLog(m.projectPath, "graph-semantic-enrichment", repo.ID, fmt.Sprintf("Added or refreshed %d LLM-inferred semantic relation(s).", count))
		}
	}
	return result, nil
}

func (m *GraphManager) ensureManagedCheckout(ctx context.Context, registry config.CodeRegistryConfig, repo config.CodeRepositoryConfig, commit string) (string, error) {
	root, err := filepath.Abs(m.config.CheckoutRoot)
	if err != nil {
		return "", err
	}
	checkout := filepath.Join(root, filepath.FromSlash(registry.Directory), repo.ID)
	if !isPathInside(root, checkout) {
		return "", fmt.Errorf("managed checkout escapes graph.checkout_root")
	}
	if info, err := os.Lstat(checkout); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("managed checkout is not a regular directory: %s", checkout)
		}
		marker, err := readManagedRepoMarker(checkout)
		if err != nil {
			return "", fmt.Errorf("refusing to use unowned checkout %s: %w", checkout, err)
		}
		if marker.RegistryID != registry.ID || marker.RepositoryID != repo.ID || canonicalGitURL(marker.CloneURL) != canonicalGitURL(repo.CloneURL) {
			return "", fmt.Errorf("managed checkout ownership mismatch: %s", checkout)
		}
		remote, err := runGit(ctx, checkout, registry, "remote", "get-url", "origin")
		if err != nil || canonicalGitURL(strings.TrimSpace(remote)) != canonicalGitURL(repo.CloneURL) {
			return "", fmt.Errorf("managed checkout origin does not match configured repository")
		}
		if _, err := runGit(ctx, checkout, registry, "fetch", "--prune", "origin", "+refs/heads/"+repo.Branch+":refs/remotes/origin/"+repo.Branch); err != nil {
			return "", err
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(checkout), 0o755); err != nil {
			return "", err
		}
		if _, err := runGit(ctx, "", registry, "clone", "--no-tags", "--single-branch", "--branch", repo.Branch, repo.CloneURL, checkout); err != nil {
			return "", err
		}
		marker := managedRepoMarker{Version: 1, RegistryID: registry.ID, RepositoryID: repo.ID, CloneURL: repo.CloneURL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		data, _ := json.MarshalIndent(marker, "", "  ")
		data = append(data, '\n')
		if err := writeFileAtomic(filepath.Join(checkout, ".kbcore-managed-repo.json"), data); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	target := "origin/" + repo.Branch
	if strings.TrimSpace(commit) != "" {
		if _, err := runGit(ctx, checkout, registry, "cat-file", "-e", strings.TrimSpace(commit)+"^{commit}"); err != nil {
			return "", fmt.Errorf("webhook commit is not available from allowlisted branch: %w", err)
		}
		if _, err := runGit(ctx, checkout, registry, "merge-base", "--is-ancestor", strings.TrimSpace(commit), "origin/"+repo.Branch); err != nil {
			return "", fmt.Errorf("webhook commit is outside the allowlisted branch")
		}
		target = strings.TrimSpace(commit)
	}
	if _, err := runGit(ctx, checkout, registry, "checkout", "--detach", target); err != nil {
		return "", err
	}
	return checkout, nil
}

func runGit(ctx context.Context, directory string, registry config.CodeRegistryConfig, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if directory != "" {
		cmd.Dir = directory
	}
	env := append([]string(nil), os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	authValue := ""
	if registry.APIToken != "" {
		username := "oauth2"
		if registry.Provider == "github" {
			username = "x-access-token"
		}
		auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + registry.APIToken))
		authValue = auth
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
		)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if registry.APIToken != "" {
			message = strings.ReplaceAll(message, registry.APIToken, "[redacted]")
		}
		if authValue != "" {
			message = strings.ReplaceAll(message, authValue, "[redacted]")
		}
		if len(message) > 2000 {
			message = message[:2000]
		}
		return "", fmt.Errorf("git %s: %w: %s", firstNonEmptyString(firstArg(args), "command"), err, message)
	}
	return string(out), nil
}

func (m *GraphManager) lookupRepository(registryID, repositoryID, fullName string) (config.CodeRegistryConfig, config.CodeRepositoryConfig, error) {
	for _, registry := range m.config.Registries {
		if registry.ID != registryID {
			continue
		}
		for _, repo := range registry.Repositories {
			if repo.Disabled {
				continue
			}
			if (repositoryID != "" && repo.ID == repositoryID) || (fullName != "" && strings.EqualFold(repo.FullName, fullName)) {
				return registry, repo, nil
			}
		}
		return config.CodeRegistryConfig{}, config.CodeRepositoryConfig{}, fmt.Errorf("repository is not allowlisted in registry %s", registryID)
	}
	return config.CodeRegistryConfig{}, config.CodeRepositoryConfig{}, fmt.Errorf("unknown graph registry %q", registryID)
}

func (m *GraphManager) lookupRegistry(registryID string) (config.CodeRegistryConfig, bool, error) {
	for _, registry := range m.config.Registries {
		if registry.ID == registryID {
			return registry, true, nil
		}
	}
	return config.CodeRegistryConfig{}, false, fmt.Errorf("unknown graph registry %q", registryID)
}

func (m *GraphManager) claimPendingJobs() ([]GraphIndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue, err := m.loadQueueLocked()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var jobs []GraphIndexJob
	for index := range queue.Jobs {
		if len(jobs) >= m.config.MaxParallelJobs {
			break
		}
		if queue.Jobs[index].Status != GraphJobPending {
			continue
		}
		queue.Jobs[index].Status = GraphJobRunning
		queue.Jobs[index].Attempts++
		queue.Jobs[index].UpdatedAt = now
		jobs = append(jobs, queue.Jobs[index])
	}
	if len(jobs) > 0 {
		if err := m.saveQueueLocked(queue); err != nil {
			return nil, err
		}
	}
	return jobs, nil
}

func (m *GraphManager) finishJob(id string, result GoCodeIndexResult, processErr error) (GraphIndexJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue, err := m.loadQueueLocked()
	if err != nil {
		return GraphIndexJob{}, err
	}
	for index := range queue.Jobs {
		job := &queue.Jobs[index]
		if job.ID != id {
			continue
		}
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if processErr != nil {
			job.Status = GraphJobFailed
			job.Error = redactGraphError(processErr.Error(), m.config)
		} else {
			job.Status = GraphJobDone
			job.Error = ""
			job.Commit = result.Commit
			job.SnapshotPath = result.SnapshotPath
			job.NodeCount = result.Nodes
			job.EdgeCount = result.Edges
		}
		if err := m.saveQueueLocked(queue); err != nil {
			return GraphIndexJob{}, err
		}
		return *job, nil
	}
	return GraphIndexJob{}, fmt.Errorf("graph job not found: %s", id)
}

func (m *GraphManager) recoverQueue() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	queue, err := m.loadQueueLocked()
	if err != nil {
		return err
	}
	changed := false
	now := time.Now().UTC().Format(time.RFC3339)
	for index := range queue.Jobs {
		if queue.Jobs[index].Status == GraphJobRunning {
			queue.Jobs[index].Status = GraphJobPending
			queue.Jobs[index].Error = "recovered after service restart"
			queue.Jobs[index].UpdatedAt = now
			changed = true
		}
	}
	if changed {
		return m.saveQueueLocked(queue)
	}
	return nil
}

func (m *GraphManager) loadQueueLocked() (graphJobQueue, error) {
	queue := graphJobQueue{Version: 1, Jobs: []GraphIndexJob{}}
	data, err := os.ReadFile(m.queuePath())
	if os.IsNotExist(err) {
		return queue, nil
	}
	if err != nil {
		return graphJobQueue{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return queue, nil
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		return graphJobQueue{}, fmt.Errorf("read graph job queue: %w", err)
	}
	if queue.Version != 1 {
		return graphJobQueue{}, fmt.Errorf("unsupported graph job queue version %d", queue.Version)
	}
	return queue, nil
}

func (m *GraphManager) saveQueueLocked(queue graphJobQueue) error {
	queue.Version = 1
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(m.queuePath()), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(m.queuePath(), data)
}

func (m *GraphManager) queuePath() string {
	return filepath.Join(m.projectPath, ".kbcore", "graph-jobs.json")
}

func readManagedRepoMarker(checkout string) (managedRepoMarker, error) {
	data, err := os.ReadFile(filepath.Join(checkout, ".kbcore-managed-repo.json"))
	if err != nil {
		return managedRepoMarker{}, err
	}
	var marker managedRepoMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return managedRepoMarker{}, err
	}
	return marker, nil
}

func canonicalGitURL(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".git")
	return strings.TrimRight(value, "/")
}

func redactGraphError(message string, graph config.GraphConfig) string {
	for _, registry := range graph.Registries {
		for _, secret := range []string{registry.APIToken, registry.WebhookSecret} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
	}
	return message
}

func firstArg(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func graphDeliveryHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:24]
}

func verifyRegistryWebhook(registry config.CodeRegistryConfig, headers http.Header, body []byte) error {
	secret := []byte(registry.WebhookSecret)
	if len(secret) == 0 {
		return fmt.Errorf("webhook secret is not configured")
	}
	switch registry.Provider {
	case "github":
		value := strings.TrimSpace(headers.Get("X-Hub-Signature-256"))
		if !strings.HasPrefix(value, "sha256=") || !verifyHexHMAC(secret, body, strings.TrimPrefix(value, "sha256=")) {
			return fmt.Errorf("invalid webhook signature")
		}
	case "gitea":
		if !verifyHexHMAC(secret, body, strings.TrimSpace(headers.Get("X-Gitea-Signature"))) {
			return fmt.Errorf("invalid webhook signature")
		}
	case "gitlab":
		if !hmac.Equal([]byte(strings.TrimSpace(headers.Get("X-Gitlab-Token"))), secret) {
			return fmt.Errorf("invalid webhook token")
		}
	default:
		return fmt.Errorf("unsupported webhook provider %q", registry.Provider)
	}
	return nil
}

func verifyHexHMAC(secret, body []byte, value string) bool {
	provided, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write(body)
	return hmac.Equal(provided, h.Sum(nil))
}

func parseRegistryPush(provider string, headers http.Header, body []byte) (fullName, branch, commit, delivery, event string, err error) {
	var payload struct {
		Ref         string `json:"ref"`
		After       string `json:"after"`
		CheckoutSHA string `json:"checkout_sha"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return "", "", "", "", "", fmt.Errorf("invalid webhook JSON: %w", err)
	}
	fullName = firstNonEmptyString(payload.Repository.FullName, payload.Project.PathWithNamespace)
	if strings.HasPrefix(payload.Ref, "refs/heads/") {
		branch = strings.TrimPrefix(payload.Ref, "refs/heads/")
	}
	commit = firstNonEmptyString(payload.CheckoutSHA, payload.After)
	switch provider {
	case "github":
		delivery = headers.Get("X-GitHub-Delivery")
		if strings.EqualFold(headers.Get("X-GitHub-Event"), "push") {
			event = "push"
		}
	case "gitlab":
		delivery = firstNonEmptyString(headers.Get("X-Gitlab-Event-UUID"), headers.Get("X-Gitlab-Webhook-UUID"))
		if strings.Contains(strings.ToLower(headers.Get("X-Gitlab-Event")), "push") {
			event = "push"
		}
	case "gitea":
		delivery = headers.Get("X-Gitea-Delivery")
		if strings.EqualFold(headers.Get("X-Gitea-Event"), "push") {
			event = "push"
		}
	}
	return fullName, branch, commit, delivery, event, nil
}
