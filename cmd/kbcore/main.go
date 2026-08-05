package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/api"
	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/grpcapi"
	"github.com/hejw/knowledge-core/internal/llmclient"
	"github.com/hejw/knowledge-core/internal/postgres"
	"github.com/hejw/knowledge-core/internal/scope"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

var runtimeConfig config.Config

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	configPath, commandArgs, err := extractConfigFlag(args)
	if err != nil {
		return err
	}
	if len(commandArgs) == 0 {
		usage()
		return nil
	}
	if commandArgs[0] != "help" && commandArgs[0] != "-h" && commandArgs[0] != "--help" && commandArgs[0] != "migrate-sql" {
		runtimeConfig, err = config.Load(configPath)
		if err != nil {
			return err
		}
		llmclient.SetGlobalConcurrency(configuredLLMConcurrency(runtimeConfig))
	}
	args = commandArgs
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "ingest":
		return runIngest(args[1:])
	case "validate-llmwiki":
		return runValidateLLMWiki(args[1:])
	case "audit-wiki":
		return runAuditWiki(args[1:])
	case "repair-wiki":
		return runRepairWiki(args[1:])
	case "refresh-overview":
		return runRefreshOverview(args[1:])
	case "queue-ingest":
		return runQueueIngest(args[1:])
	case "scan-sources":
		return runScanSources(args[1:])
	case "source-layout":
		return runSourceLayout(args[1:])
	case "run-queue":
		return runRunQueue(args[1:])
	case "maintain":
		return runMaintain(args[1:])
	case "query":
		return runQuery(args[1:])
	case "sync-wiki-pg":
		return runSyncWikiPG(args[1:])
	case "lint":
		return runLint(args[1:])
	case "review-wiki":
		return runReviewWiki(args[1:])
	case "review-tasks":
		return runReviewTasks(args[1:])
	case "resolve-review":
		return runResolveReview(args[1:])
	case "code-import-graphify":
		return runCodeImportGraphify(args[1:])
	case "code-index-go":
		return runCodeIndexGo(args[1:])
	case "migrate-sql":
		fmt.Print(postgres.BootstrapSQL)
		return nil
	case "mcp":
		return runMCP(args[1:])
	case "serve":
		return runServe(args[1:])
	case "wait-ready":
		return runWaitReady(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func configuredLLMConcurrency(cfg config.Config) int {
	if cfg.LLM.Concurrency > 0 {
		return cfg.LLM.Concurrency
	}
	return cfg.Project.Bootstrap.Concurrency
}

func runRepairWiki(args []string) error {
	fs := flag.NewFlagSet("repair-wiki", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	apply := fs.Bool("apply", false, "apply versioned repairs; default is a read-only audit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*apply {
		audit, err := service.AuditWikiQuality(*project)
		if err != nil {
			return err
		}
		fmt.Printf("dry_run=true\nready=%t\ninvalid_provenance=%d\nnoncanonical_pages=%d\nunowned_pages=%d\nindex_gaps=%d\nalias_conflicts=%d\ninvalid_reviews=%d\nlint_issues=%d\n",
			audit.Ready, len(audit.InvalidProvenance), len(audit.NonCanonicalPages), len(audit.UnownedPages), len(audit.IndexMissingPages), len(audit.AliasConflicts), len(audit.InvalidReviewItems), len(audit.LintIssues))
		return nil
	}
	result, err := compiler.ConvergeWikiArtifacts(*project)
	if err != nil {
		return err
	}
	linkedMentions, err := service.RepairKnownMentionLinks(*project)
	if err != nil {
		return err
	}
	if err := wiki.RebuildIndex(*project); err != nil {
		return err
	}
	if err := service.RefreshRelationsArtifact(*project); err != nil {
		return err
	}
	fmt.Printf("rewritten_pages=%d\nmoved_pages=%d\nmerged_pages=%d\nremoved_bad_sources=%d\nremoved_ambiguous_aliases=%d\nnormalized_reviews=%d\ndowngraded_links=%d\nlinked_summaries=%d\nlinked_mentions=%d\nquarantined_pages=%d\nunowned_pages=%d\n",
		result.RewrittenPages, result.MovedPages, result.MergedPages, result.RemovedBadSources,
		result.RemovedAmbiguousAliases, result.NormalizedReviews, result.DowngradedLinks, result.LinkedSummaries, linkedMentions, result.QuarantinedPages, result.UnownedPages)
	return nil
}

func runRefreshOverview(args []string) error {
	fs := flag.NewFlagSet("refresh-overview", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "overview agent: llm, mock")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider, err := ingestProvider(*agentName)
	if err != nil {
		return err
	}
	if *agentName == "llm" {
		if err := wiki.ValidatePurposeReady(*project); err != nil {
			return err
		}
	}
	if _, err := compiler.ConvergeWikiArtifacts(*project); err != nil {
		return err
	}
	linked, err := service.RepairKnownMentionLinks(*project)
	if err != nil {
		return err
	}
	if err := wiki.RebuildIndex(*project); err != nil {
		return err
	}
	refreshed, err := compiler.RefreshOverview(provider, *project)
	if err != nil {
		return err
	}
	if err := service.RefreshRelationsArtifact(*project); err != nil {
		return err
	}
	audit, err := service.ValidateWikiQualityReady(*project)
	if err != nil {
		return err
	}
	fmt.Printf("overview_refreshed=%t\nlinked_mentions=%d\nready=%t\n", refreshed, linked, audit.Ready)
	return nil
}

func runAuditWiki(args []string) error {
	fs := flag.NewFlagSet("audit-wiki", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	baseline := fs.String("baseline", "", "optional baseline project path")
	report := fs.String("report", "", "optional Markdown report path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	current, err := service.AuditWikiQuality(*project)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	if strings.TrimSpace(*baseline) == "" {
		return nil
	}
	old, err := service.AuditWikiQuality(*baseline)
	if err != nil {
		return err
	}
	path := strings.TrimSpace(*report)
	if path == "" {
		path = filepath.Join(*project, "COMPARE_REPORT.md")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(service.AuditComparisonMarkdown(old, current)), 0o644); err != nil {
		return err
	}
	fmt.Printf("report=%s\n", path)
	return nil
}

func runWaitReady(args []string) error {
	fs := flag.NewFlagSet("wait-ready", flag.ContinueOnError)
	addr := fs.String("addr", runtimeConfig.Server.Addr, "server address to wait for")
	timeout := fs.Duration("timeout", 2*time.Hour, "maximum time to wait")
	interval := fs.Duration("interval", 500*time.Millisecond, "poll interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("wait-ready addr: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	healthURL := "http://" + net.JoinHostPort(host, port) + "/health"
	deadline := time.Now().Add(*timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, requestErr := client.Get(healthURL)
		if requestErr == nil {
			var health struct {
				Ready bool `json:"ready"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&health)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && health.Ready {
				fmt.Println("backend_ready", healthURL)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for backend readiness at %s", healthURL)
		}
		time.Sleep(*interval)
	}
}

func extractConfigFlag(args []string) (string, []string, error) {
	path := ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--config" {
			if path != "" {
				return "", nil, fmt.Errorf("--config may only be specified once")
			}
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", nil, fmt.Errorf("--config requires a path")
			}
			path = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--config=") {
			if path != "" {
				return "", nil, fmt.Errorf("--config may only be specified once")
			}
			path = strings.TrimSpace(strings.TrimPrefix(arg, "--config="))
			if path == "" {
				return "", nil, fmt.Errorf("--config requires a path")
			}
			continue
		}
		out = append(out, arg)
	}
	return path, out, nil
}

func runValidateLLMWiki(args []string) error {
	fs := flag.NewFlagSet("validate-llmwiki", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	source := fs.String("source", "", "source file or directory")
	title := fs.String("title", "", "source title")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "ingest agent: llm, mock")
	skipUnchanged := fs.Bool("skip-unchanged", false, "skip sources whose content hash is unchanged in the source manifest")
	showProgress := fs.Bool("progress", false, "print per-source task phases, retries, and validation errors")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for wiki sync")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider, err := ingestProvider(*agentName)
	if err != nil {
		return err
	}
	var onProgress func(compiler.ValidateProgress)
	if *showProgress {
		onProgress = logBootstrapProgress
	}
	result, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath:          *project,
		SourcePath:           *source,
		Title:                *title,
		Provider:             provider,
		SkipUnchanged:        *skipUnchanged,
		Concurrency:          runtimeConfig.Project.Bootstrap.Concurrency,
		LLMConcurrency:       configuredLLMConcurrency(runtimeConfig),
		MaxTaskAttempts:      runtimeConfig.Project.Bootstrap.MaxTaskAttempts,
		MaxConflictAttempts:  runtimeConfig.Project.Bootstrap.MaxConflictAttempts,
		MaxImpactAttempts:    runtimeConfig.Project.Bootstrap.MaxImpactAttempts,
		MaxFilesPerTask:      runtimeConfig.Project.Bootstrap.MaxFilesPerTask,
		MaxNewPagesPerSource: runtimeConfig.Project.Bootstrap.MaxNewPagesPerSource,
		ImpactAssessor:       impactAssessorFromProvider(provider),
		OnProgress:           onProgress,
	})
	if err != nil {
		return err
	}
	if _, err := compiler.ConvergeWikiArtifacts(*project); err != nil {
		return err
	}
	if _, err := service.RepairKnownMentionLinks(*project); err != nil {
		return err
	}
	if err := wiki.RebuildIndex(*project); err != nil {
		return err
	}
	if _, err := compiler.RefreshOverview(provider, *project); err != nil {
		return err
	}
	if err := service.RefreshRelationsArtifact(*project); err != nil {
		return err
	}
	if _, err := service.ValidateWikiReady(*project, *source); err != nil {
		return err
	}
	fmt.Printf("sources=%d\nfiles=%d\nreviews=%d\nskipped=%d\n", result.SourceCount, result.FileCount, result.ReviewCount, result.SkippedCount)
	for _, item := range result.Results {
		if item.Skipped {
			fmt.Printf("skipped=%s\n", item.RawPath)
			continue
		}
		fmt.Printf("raw=%s\n", item.RawPath)
		for _, file := range item.Files {
			fmt.Printf("file=%s\n", file)
		}
	}
	if err := syncWrittenWikiPages(context.Background(), *project, *dbDSN, *projectIDFlag, batchWrittenWikiPaths(result)); err != nil {
		return err
	}
	return nil
}

func ingestProvider(agentName string) (compiler.Provider, error) {
	switch agentName {
	case "llm":
		return compiler.NewProvider(runtimeConfig.LLM)
	case "mock":
		return compiler.MockProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown ingest agent %q", agentName)
	}
}

func runQueueIngest(args []string) error {
	fs := flag.NewFlagSet("queue-ingest", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	source := fs.String("source", "", "source file")
	title := fs.String("title", "", "source title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task, err := service.QueueIngestSource(service.QueueIngestOptions{
		ProjectPath: *project,
		SourcePath:  *source,
		Title:       *title,
	})
	if err != nil {
		return err
	}
	fmt.Printf("queued=%s\nsource=%s\nstatus=%s\nsha256=%s\n", task.ID, task.SourcePath, task.Status, task.SHA256)
	return nil
}

func runScanSources(args []string) error {
	fs := flag.NewFlagSet("scan-sources", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := service.ScanRawSources(service.QueueIngestOptions{ProjectPath: *project})
	if err != nil {
		return err
	}
	fmt.Printf("queued=%d\nskipped=%d\n", result.Queued, result.Skipped)
	for _, task := range result.Tasks {
		fmt.Printf("task=%s source=%s status=%s\n", task.ID, task.SourcePath, task.Status)
	}
	return nil
}

func runSourceLayout(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("source-layout requires migrate or status")
	}
	switch args[0] {
	case "status":
		fs := flag.NewFlagSet("source-layout status", flag.ContinueOnError)
		project := fs.String("project", runtimeConfig.Project.Path, "project path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		result, err := service.SourceLayoutMigrationStatus(*project)
		if err != nil {
			return err
		}
		printSourceLayoutMigration(result)
		return nil
	case "migrate":
		fs := flag.NewFlagSet("source-layout migrate", flag.ContinueOnError)
		project := fs.String("project", runtimeConfig.Project.Path, "project path")
		apply := fs.Bool("apply", false, "apply the migration; default is a read-only dry run")
		dryRun := fs.Bool("dry-run", false, "explicitly run the read-only migration preview")
		dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for derived-state rebuild")
		projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
		migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before rebuilding derived state")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *apply && *dryRun {
			return fmt.Errorf("--apply and --dry-run are mutually exclusive")
		}
		result, err := service.MigrateSourceLayout(service.SourceLayoutMigrationOptions{ProjectPath: *project, Apply: *apply})
		printSourceLayoutMigration(result)
		if err != nil {
			return err
		}
		if *apply && strings.TrimSpace(*dbDSN) != "" {
			ctx := context.Background()
			projectID, err := requireProjectID(*projectIDFlag)
			if err != nil {
				return err
			}
			dbHandle, err := openDBStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer dbHandle.Close()
			if *migrateDB {
				if err := dbHandle.store.Migrate(ctx); err != nil {
					return fmt.Errorf("migrate PostgreSQL before source layout sync: %w", err)
				}
			}
			if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
				return err
			}
			if _, err := service.SyncWikiPagesToStore(ctx, service.WikiSyncOptions{ProjectPath: *project, ProjectID: projectID, Store: dbHandle.store}); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown source-layout command %q", args[0])
	}
}

func printSourceLayoutMigration(result service.SourceLayoutMigrationResult) {
	fmt.Printf("status=%s\ndry_run=%t\ntotal=%d\ncompleted=%d\nblocked=%d\njournal=%s\n", result.Status, result.DryRun, result.Total, result.Completed, result.Blocked, result.JournalPath)
	for _, item := range result.Items {
		fmt.Printf("source=%s target=%s stage=%s", item.OriginalSource, item.TargetArchivePath, item.Stage)
		if item.Error != "" {
			fmt.Printf(" error=%s", item.Error)
		}
		fmt.Println()
	}
}

func runRunQueue(args []string) error {
	fs := flag.NewFlagSet("run-queue", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "ingest agent: llm, mock")
	maxTasks := fs.Int("max", 0, "maximum tasks to process; 0 means all")
	skipUnchanged := fs.Bool("skip-unchanged", true, "skip sources whose content hash is unchanged in the source manifest")
	retryFailed := fs.Bool("retry-failed", false, "retry failed tasks")
	keepDone := fs.Bool("keep-done", false, "keep completed tasks in .kbcore/ingest-queue.json")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for wiki sync")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider, err := ingestProvider(*agentName)
	if err != nil {
		return err
	}
	result, err := service.RunIngestQueue(service.RunIngestQueueOptions{
		ProjectPath:   *project,
		Validator:     compilerQueueValidator(provider),
		SkipUnchanged: *skipUnchanged,
		MaxTasks:      *maxTasks,
		RetryFailed:   *retryFailed,
		KeepDone:      *keepDone,
	})
	if err != nil {
		return err
	}
	if err := service.RefreshRelationsArtifact(*project); err != nil {
		return err
	}
	fmt.Printf("processed=%d\ndone=%d\nfailed=%d\nskipped=%d\nfiles=%d\n", result.Processed, result.Done, result.Failed, result.Skipped, result.Files)
	for _, task := range result.Tasks {
		fmt.Printf("task=%s source=%s status=%s retries=%d\n", task.ID, task.SourcePath, task.Status, task.RetryCount)
		if task.Error != "" {
			fmt.Printf("error=%s\n", task.Error)
		}
	}
	if err := syncWrittenWikiPages(context.Background(), *project, *dbDSN, *projectIDFlag, []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}); err != nil {
		return err
	}
	return nil
}

func runMaintain(args []string) error {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "agent for ingest/review/sweep: llm")
	skipUnchanged := fs.Bool("skip-unchanged", true, "skip sources whose content hash is unchanged")
	retryFailed := fs.Bool("retry-failed", false, "retry failed ingest queue tasks")
	keepDone := fs.Bool("keep-done", false, "keep completed tasks in .kbcore/ingest-queue.json")
	runReview := fs.Bool("review", false, "run LLM semantic wiki review")
	runSyncPG := fs.Bool("sync-pg", false, "sync wiki pages, reviews, sources, and embeddings to PostgreSQL")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for sync")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider, err := ingestProvider(*agentName)
	if err != nil {
		return err
	}
	var reviewAgent service.WikiReviewAgent
	var reviewAgentErr error
	if *runReview {
		reviewAgent, reviewAgentErr = service.NewWikiReviewAgent(runtimeConfig.LLM)
	}
	var sweepAgent service.QueryAgent
	switch *agentName {
	case "llm":
		configuredAgent, err := service.NewQueryAgent(runtimeConfig.LLM)
		if err != nil {
			return err
		}
		sweepAgent = configuredAgent
	default:
		return fmt.Errorf("unknown agent %q", *agentName)
	}
	projectID := *projectIDFlag
	ctx := context.Background()
	var dbHandle *dbStoreHandle
	var wikiStore service.WikiPageStore
	var embeddingProvider service.EmbeddingProvider
	if *runSyncPG {
		dbHandle, err = openDBStore(ctx, *dbDSN)
		if err != nil {
			return err
		}
		if dbHandle != nil {
			defer dbHandle.Close()
			projectID, err = requireProjectID(projectID)
			if err != nil {
				return err
			}
			if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
				return err
			}
			wikiStore = dbHandle.store
			configuredEmbedding, ok, err := service.NewEmbeddingProvider(runtimeConfig.Embedding)
			if err != nil {
				return err
			}
			if ok {
				embeddingProvider = configuredEmbedding
			}
		}
	}
	result, err := service.MaintainWiki(service.MaintainWikiOptions{
		ProjectPath:       *project,
		ProjectID:         projectID,
		QueueValidator:    compilerQueueValidator(provider),
		SkipUnchanged:     *skipUnchanged,
		RetryFailed:       *retryFailed,
		KeepDone:          *keepDone,
		RunLLMReview:      *runReview,
		ReviewAgent:       reviewAgent,
		ReviewAgentError:  reviewAgentErr,
		SweepAgent:        sweepAgent,
		RunPGSync:         *runSyncPG,
		WikiStore:         wikiStore,
		EmbeddingProvider: embeddingProvider,
		Context:           ctx,
	})
	if err != nil {
		return err
	}
	fmt.Printf("status=%s\n", result.Status)
	for _, step := range result.Steps {
		fmt.Printf("step=%s status=%s", step.Name, step.Status)
		for key, value := range step.Summary {
			fmt.Printf(" %s=%v", key, value)
		}
		fmt.Println()
		if step.Error != "" {
			fmt.Printf("error=%s\n", step.Error)
		}
	}
	if result.Status == "failed" {
		return fmt.Errorf("maintenance failed")
	}
	return nil
}

func compilerQueueValidator(provider compiler.Provider) service.IngestQueueValidator {
	return func(opts service.QueueValidateOptions) (service.QueueValidateResult, error) {
		result, err := compiler.ValidateLLMWiki(compiler.ValidateOptions{
			ProjectPath:   opts.ProjectPath,
			SourcePath:    opts.SourcePath,
			Title:         opts.Title,
			Provider:      provider,
			SkipUnchanged: opts.SkipUnchanged,
		})
		if err != nil {
			return service.QueueValidateResult{}, err
		}
		return service.QueueValidateResult{
			RawPath: result.RawPath,
			Files:   result.Files,
			Skipped: result.Skipped,
			SHA256:  result.SHA256,
		}, nil
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", runtimeConfig.Server.Addr, "listen address")
	project := fs.String("project", runtimeConfig.Project.Path, "default project path for API requests")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "default agent for service operations: llm")
	worker := fs.Bool("worker", runtimeConfig.Server.Worker, "run background source scan and ingest queue worker")
	scanInterval := fs.Duration("scan-interval", runtimeConfig.Server.ScanInterval.Duration, "background worker scan interval")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for graph evidence")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "default PostgreSQL project id")
	migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before starting the service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	explicit := explicitFlags(fs)
	effectiveConfig := runtimeConfig
	if explicit["project"] {
		effectiveConfig.Project.Path = *project
	}
	if explicit["addr"] {
		effectiveConfig.Server.Addr = *addr
	}
	if explicit["agent"] {
		effectiveConfig.Server.Agent = *agentName
	}
	if explicit["worker"] {
		effectiveConfig.Server.Worker = *worker
	}
	if explicit["scan-interval"] {
		effectiveConfig.Server.ScanInterval.Duration = *scanInterval
	}
	if explicit["db-dsn"] {
		effectiveConfig.Database.DSN = *dbDSN
	}
	if explicit["project-id"] {
		effectiveConfig.Database.ProjectID = *projectIDFlag
	}
	if err := effectiveConfig.Validate(); err != nil {
		return err
	}
	if err := effectiveConfig.ValidateServe(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbHandle, err := openDBStore(ctx, *dbDSN)
	if err != nil {
		return fmt.Errorf("database preflight: %w", err)
	}
	defer dbHandle.Close()
	projectID := *projectIDFlag
	if dbHandle != nil {
		if *migrateDB {
			if err := dbHandle.store.Migrate(ctx); err != nil {
				return fmt.Errorf("database migration: %w", err)
			}
		}
		projectID, err = requireProjectID(projectID)
		if err != nil {
			return err
		}
	}
	configuredEmbedding, embeddingConfigured, err := service.NewEmbeddingProvider(effectiveConfig.Embedding)
	if err != nil {
		return err
	}
	var embeddingProvider service.EmbeddingProvider
	if embeddingConfigured {
		embeddingProvider = configuredEmbedding
	}

	var graphStore service.GraphEvidenceStore
	var codeGraphStore service.CodeGraphStore
	var searchStore service.SearchEvidenceStore
	var wikiPageStore service.WikiPageStore
	var queryLogStore service.QueryLogStore
	if dbHandle != nil {
		if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
			return err
		}
		graphStore = dbHandle.store
		codeGraphStore = dbHandle.store
		searchStore = dbHandle.store
		wikiPageStore = dbHandle.store
		queryLogStore = dbHandle.store
	}
	var graphManager *service.GraphManager
	if effectiveConfig.Graph.Enabled {
		var graphSemanticEnricher service.GraphSemanticEnricher
		if effectiveConfig.Graph.SemanticEnrichment {
			graphSemanticEnricher, err = service.NewLLMGraphSemanticEnricher(effectiveConfig.LLM)
			if err != nil {
				return fmt.Errorf("graph semantic enricher: %w", err)
			}
		}
		graphManager, err = service.NewGraphManager(service.GraphManagerOptions{
			ProjectPath: *project, ProjectID: projectID, Config: effectiveConfig.Graph, Store: codeGraphStore,
			WikiStore: wikiPageStore, EmbeddingProvider: embeddingProvider, SemanticEnricher: graphSemanticEnricher,
		})
		if err != nil {
			return fmt.Errorf("graph manager: %w", err)
		}
	}
	ingestLLMProvider, err := compiler.NewProvider(effectiveConfig.LLM)
	if err != nil {
		return err
	}
	queryAgent, err := service.NewQueryAgent(effectiveConfig.LLM)
	if err != nil {
		return err
	}
	reviewAgent, err := service.NewWikiReviewAgent(effectiveConfig.LLM)
	if err != nil {
		return err
	}
	totalSources, err := service.CountBootstrapSources(effectiveConfig.Project.Bootstrap.Source)
	if err != nil {
		return err
	}
	tracker := service.NewBootstrapTracker(*project, totalSources)
	expectedContract := ""
	if contract, contractErr := compiler.GenerationContractSHA256(*project, effectiveConfig.Project.Bootstrap.MaxFilesPerTask, effectiveConfig.Project.Bootstrap.MaxNewPagesPerSource); contractErr == nil {
		expectedContract = contract
	}
	if restoreErr := service.RestoreBootstrapTracker(tracker, *project, effectiveConfig.Project.Bootstrap.Source, expectedContract); restoreErr != nil {
		return fmt.Errorf("restore bootstrap progress: %w", restoreErr)
	}
	var grpcListener net.Listener
	var grpcErrCh chan error
	if effectiveConfig.Server.GRPC.Enabled {
		var bindingStore scope.BindingStore
		var documentCounter grpcapi.ProjectDocumentCounter
		if dbHandle != nil {
			bindingStore = dbHandle.store
			documentCounter = dbHandle.store
		}
		resolver, resolverErr := scope.NewResolver(scope.Options{
			Provider:  "qm",
			ScopeRoot: effectiveConfig.Server.GRPC.ScopeRoot,
			Store:     bindingStore,
		})
		if resolverErr != nil {
			return fmt.Errorf("create QM scope resolver: %w", resolverErr)
		}
		defaultScopeID := "group:project:" + projectID
		if err := resolver.Register(ctx, core.ScopeBinding{
			Provider:        "qm",
			ExternalScopeID: defaultScopeID,
			Kind:            "project",
			ProjectID:       projectID,
			ProjectName:     effectiveConfig.Project.Name,
			RootPath:        *project,
			Status:          "active",
		}); err != nil {
			return fmt.Errorf("bind default QM scope: %w", err)
		}
		grpcCreds, credsErr := grpcTransportCredentials(effectiveConfig.Server.GRPC)
		if credsErr != nil {
			return credsErr
		}
		grpcService, grpcErr := grpcapi.NewServer(grpcapi.ServerOptions{
			Resolver:          resolver,
			SearchStore:       searchStore,
			GraphStore:        graphStore,
			QueryLogStore:     queryLogStore,
			QueryAgent:        queryAgent,
			EmbeddingProvider: embeddingProvider,
			Runtime:           queryRuntimeOptions(effectiveConfig.Query),
			Bootstrap:         tracker,
			DocumentCounter:   documentCounter,
			AuthToken:         effectiveConfig.Server.GRPC.AuthToken,
			RequireAuth:       effectiveConfig.Server.GRPC.RequireAuth,
			TransportCreds:    grpcCreds,
		})
		if grpcErr != nil {
			return fmt.Errorf("create QM gRPC server: %w", grpcErr)
		}
		grpcListener, err = net.Listen("tcp", effectiveConfig.Server.GRPC.Addr)
		if err != nil {
			return fmt.Errorf("listen gRPC %s: %w", effectiveConfig.Server.GRPC.Addr, err)
		}
		grpcErrCh = make(chan error, 1)
		go func() {
			grpcErrCh <- grpcService.Serve(ctx, grpcListener)
		}()
		fmt.Println("serving grpc", grpcListener.Addr().String())
	}
	server := &http.Server{
		Addr: *addr,
		Handler: api.NewServerWithOptions(api.ServerOptions{
			SearchStore:       searchStore,
			GraphStore:        graphStore,
			CodeGraphStore:    codeGraphStore,
			WikiPageStore:     wikiPageStore,
			QueryLogStore:     queryLogStore,
			QueryAgent:        queryAgent,
			IngestProvider:    ingestLLMProvider,
			ReviewAgent:       reviewAgent,
			EmbeddingProvider: embeddingProvider,
			ResearchOptions: service.ResearchOptions{
				BaseURL:    effectiveConfig.Research.SearXNGURL,
				MaxResults: effectiveConfig.Research.MaxResults,
				Timeout:    effectiveConfig.Research.Timeout.Duration,
			},
			QueryRuntime:       queryRuntimeOptions(effectiveConfig.Query),
			RequestLogger:      log.New(os.Stdout, "http ", log.LstdFlags),
			APIToken:           effectiveConfig.Server.APIToken,
			APIRequireToken:    effectiveConfig.Server.APIRequireToken,
			DefaultProjectPath: *project,
			DefaultProjectID:   projectID,
			DefaultAgent:       *agentName,
			Bootstrap:          tracker,
			GraphManager:       graphManager,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}
	fmt.Println("serving", listener.Addr().String())
	if graphManager != nil && effectiveConfig.Graph.Worker {
		go runGraphManagerWhenReady(ctx, tracker, graphManager)
	}
	go runServeBootstrap(ctx, serveBootstrapOptions{
		Config: effectiveConfig, ProjectPath: *project, ProjectID: projectID,
		Provider: ingestLLMProvider, Tracker: tracker, DBHandle: dbHandle,
		EmbeddingProvider: embeddingProvider, Worker: *worker, ScanInterval: *scanInterval,
	})
	if grpcErrCh == nil {
		return server.Serve(listener)
	}
	httpErrCh := make(chan error, 1)
	go func() {
		httpErrCh <- server.Serve(listener)
	}()
	select {
	case serveErr := <-httpErrCh:
		cancel()
		if grpcListener != nil {
			_ = grpcListener.Close()
		}
		return serveErr
	case grpcErr := <-grpcErrCh:
		cancel()
		_ = server.Close()
		if grpcErr == nil {
			return nil
		}
		return fmt.Errorf("gRPC server: %w", grpcErr)
	}
}

func runGraphManagerWhenReady(ctx context.Context, tracker *service.BootstrapTracker, manager *service.GraphManager) {
	if tracker == nil || tracker.Ready() {
		manager.Run(ctx)
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if tracker.Ready() {
				manager.Run(ctx)
				return
			}
		}
	}
}

type serveBootstrapOptions struct {
	Config            config.Config
	ProjectPath       string
	ProjectID         string
	Provider          compiler.Provider
	Tracker           *service.BootstrapTracker
	DBHandle          *dbStoreHandle
	EmbeddingProvider service.EmbeddingProvider
	Worker            bool
	ScanInterval      time.Duration
}

func runServeBootstrap(ctx context.Context, opts serveBootstrapOptions) {
	delay := opts.Config.Project.Bootstrap.RetryInitialDelay.Duration
	maxDelay := opts.Config.Project.Bootstrap.RetryMaxDelay.Duration
	attempt := 1
	lastCompleted := 0
	for {
		opts.Tracker.SetAttempt(attempt)
		opts.Tracker.SetStage("running", "project_validation")
		fmt.Printf("bootstrap status=running attempt=%d project=%s\n", attempt, opts.ProjectPath)
		expectedContract := ""
		if contract, contractErr := compiler.GenerationContractSHA256(
			opts.ProjectPath,
			opts.Config.Project.Bootstrap.MaxFilesPerTask,
			opts.Config.Project.Bootstrap.MaxNewPagesPerSource,
		); contractErr == nil {
			expectedContract = contract
		}
		prepared, err := service.PrepareProject(service.PrepareProjectOptions{
			ProjectPath:                opts.ProjectPath,
			ProjectName:                opts.Config.Project.Name,
			SourcePath:                 opts.Config.Project.Bootstrap.Source,
			ReuseExisting:              opts.Config.Project.Bootstrap.ReuseExisting,
			ExpectedGenerationContract: expectedContract,
			Compile: func(projectPath, sourcePath string) (service.ProjectCompileResult, error) {
				var onCommitted func(compiler.ValidateResult) error
				if opts.DBHandle != nil {
					onCommitted = func(result compiler.ValidateResult) error {
						paths := aggregateWikiPaths(result.Files...)
						_, err := service.SyncWikiPagePathsToStore(ctx, service.WikiSyncOptions{
							ProjectPath: projectPath, ProjectID: opts.ProjectID,
							Store: opts.DBHandle.store, EmbeddingProvider: opts.EmbeddingProvider,
						}, paths)
						if err != nil {
							return err
						}
						_, err = service.SyncSourceManifestToStore(ctx, service.WikiSyncOptions{
							ProjectPath: projectPath, ProjectID: opts.ProjectID, Store: opts.DBHandle.store,
						})
						return err
					}
				}
				batch, compileErr := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
					ProjectPath: projectPath, SourcePath: sourcePath, Provider: opts.Provider,
					SkipUnchanged:        true,
					Concurrency:          opts.Config.Project.Bootstrap.Concurrency,
					LLMConcurrency:       configuredLLMConcurrency(opts.Config),
					MaxTaskAttempts:      opts.Config.Project.Bootstrap.MaxTaskAttempts,
					MaxConflictAttempts:  opts.Config.Project.Bootstrap.MaxConflictAttempts,
					MaxImpactAttempts:    opts.Config.Project.Bootstrap.MaxImpactAttempts,
					MaxFilesPerTask:      opts.Config.Project.Bootstrap.MaxFilesPerTask,
					MaxNewPagesPerSource: opts.Config.Project.Bootstrap.MaxNewPagesPerSource,
					ImpactAssessor:       impactAssessorFromProvider(opts.Provider),
					OnCommitted:          onCommitted,
					OnLLMCall: func(metric compiler.LLMCallMetric) {
						if metric.Started {
							opts.Tracker.StartLLMCall(metric.ID, metric.StartedAt)
						} else {
							opts.Tracker.FinishLLMCall(metric.ID, metric.Duration, metric.Failed)
						}
					},
					OnProgress: func(progress compiler.ValidateProgress) {
						if progress.Phase == "completed" || progress.Phase == "skipped" {
							opts.Tracker.RecordSourceDuration(progress.SourcePath, progress.Duration)
						}
						opts.Tracker.UpdateTaskProgress(
							progress.Phase, progress.SourcePath, progress.Index, progress.Total,
							progress.Files, progress.Reviews, progress.Attempt, progress.Error,
						)
						logBootstrapProgress(progress)
					},
				})
				if compileErr != nil {
					return service.ProjectCompileResult{}, compileErr
				}
				return service.ProjectCompileResult{
					SourceCount: batch.SourceCount, FileCount: batch.FileCount,
					ReviewCount: batch.ReviewCount, WrittenPaths: batchWrittenWikiPaths(batch),
				}, nil
			},
		})
		generatedThisAttempt := !prepared.Reused || prepared.Resumed
		maintenanceChanged := false
		if err == nil {
			opts.Tracker.SetStage("running", "converging_wiki")
			fmt.Printf("bootstrap stage=converging_wiki project=%s\n", opts.ProjectPath)
			var convergence compiler.WikiConvergenceResult
			convergence, err = compiler.ConvergeWikiArtifacts(opts.ProjectPath)
			maintenanceChanged = convergence.RewrittenPages+convergence.MovedPages+convergence.MergedPages+convergence.RemovedBadSources+
				convergence.RemovedAmbiguousAliases+convergence.NormalizedReviews+convergence.DowngradedLinks+convergence.LinkedSummaries+
				convergence.QuarantinedPages > 0
		}
		if err == nil {
			opts.Tracker.SetStage("running", "repairing_links")
			fmt.Printf("bootstrap stage=repairing_links project=%s\n", opts.ProjectPath)
			var linked int
			linked, err = service.RepairKnownMentionLinks(opts.ProjectPath)
			maintenanceChanged = maintenanceChanged || linked > 0
		}
		needsAggregateRefresh := generatedThisAttempt || maintenanceChanged
		if err == nil && !needsAggregateRefresh {
			current, stateErr := wiki.AggregateStateCurrent(opts.ProjectPath)
			needsAggregateRefresh = stateErr != nil || !current
		}
		if err == nil && needsAggregateRefresh {
			opts.Tracker.SetStage("running", "rebuilding_index")
			fmt.Printf("bootstrap stage=rebuilding_index project=%s\n", opts.ProjectPath)
			err = wiki.RebuildIndex(opts.ProjectPath)
		}
		if err == nil && needsAggregateRefresh {
			opts.Tracker.SetStage("running", "synthesizing_overview")
			fmt.Printf("bootstrap stage=synthesizing_overview project=%s\n", opts.ProjectPath)
			_, err = compiler.RefreshOverview(opts.Provider, opts.ProjectPath)
		}
		if err == nil {
			err = service.RefreshRelationsArtifact(opts.ProjectPath)
		}
		if err == nil {
			_, readyErr := service.ValidateWikiReady(opts.ProjectPath, opts.Config.Project.Bootstrap.Source)
			if readyErr != nil {
				err = service.PermanentBootstrapError{Err: readyErr}
			}
		}
		if err == nil && opts.DBHandle != nil {
			opts.Tracker.SetStage("running", "syncing_pg")
			fmt.Printf("bootstrap stage=syncing_pg project=%s embedding=%t\n", opts.ProjectPath, opts.EmbeddingProvider != nil)
			_, err = service.SyncWikiPagesToStore(ctx, service.WikiSyncOptions{
				ProjectPath: opts.ProjectPath, ProjectID: opts.ProjectID,
				Store: opts.DBHandle.store, EmbeddingProvider: opts.EmbeddingProvider,
			})
		}
		if err == nil {
			opts.Tracker.Succeed(prepared.FileCount, prepared.ReviewCount)
			fmt.Printf("bootstrap status=succeeded project=%s sources=%d files=%d reviews=%d reused=%t resumed=%t\n",
				opts.ProjectPath, prepared.SourceCount, prepared.FileCount, prepared.ReviewCount, prepared.Reused, prepared.Resumed)
			if opts.Worker {
				go runServeWorker(ctx, serveWorkerOptions{
					ProjectPath: opts.ProjectPath, ProjectID: opts.ProjectID, Provider: opts.Provider,
					Interval: opts.ScanInterval, SkipUnchanged: true, DBDSN: opts.Config.Database.DSN,
				})
			}
			return
		}
		if err != nil && compiler.IsSourceTaskExhaustedError(err) {
			err = service.PermanentBootstrapError{Err: err}
		}
		if service.IsPermanentBootstrapError(err) {
			opts.Tracker.Fail(err)
			fmt.Fprintf(os.Stderr, "bootstrap status=failed retryable=false error=%v\n", err)
			return
		}
		completed := opts.Tracker.Snapshot().CompletedSources
		if completed > lastCompleted {
			delay = opts.Config.Project.Bootstrap.RetryInitialDelay.Duration
			lastCompleted = completed
		}
		next := time.Now().UTC().Add(delay)
		opts.Tracker.SetRetry(err, attempt, next)
		fmt.Fprintf(os.Stderr, "bootstrap status=retrying attempt=%d next_retry=%s error=%v\n", attempt, next.Format(time.RFC3339), err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		attempt++
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func logBootstrapProgress(progress compiler.ValidateProgress) {
	fmt.Printf("bootstrap source=%d/%d attempt=%d phase=%s path=%s duration=%s files=%d reviews=%d",
		progress.Index, progress.Total, progress.Attempt, progress.Phase, progress.SourcePath,
		progress.Duration.Round(time.Millisecond), progress.Files, progress.Reviews)
	if progress.Error != "" {
		fmt.Printf(" error=%s", progress.Error)
	}
	fmt.Println()
}

func impactAssessorFromProvider(provider compiler.Provider) compiler.ImpactAssessor {
	assessor, _ := provider.(compiler.ImpactAssessor)
	return assessor
}

func queryRuntimeOptions(cfg config.QueryConfig) service.QueryRuntimeOptions {
	return service.QueryRuntimeOptions{
		MaxSteps:            cfg.MaxSteps,
		InitialActionBudget: cfg.InitialActionBudget,
		MaxActionBudget:     cfg.MaxActionBudget,
		VerificationPasses:  cfg.VerificationPasses,
		StagnationRounds:    cfg.StagnationRounds,
		TotalTimeout:        cfg.TotalTimeout.Duration,
	}
}

func explicitFlags(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.Visit(func(item *flag.Flag) {
		out[item.Name] = true
	})
	return out
}

type serveWorkerOptions struct {
	ProjectPath   string
	ProjectID     string
	Provider      compiler.Provider
	Interval      time.Duration
	SkipUnchanged bool
	DBDSN         string
}

func runServeWorker(ctx context.Context, opts serveWorkerOptions) {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	run := func() {
		if _, err := service.ScanRawSources(service.QueueIngestOptions{ProjectPath: opts.ProjectPath}); err != nil {
			fmt.Fprintln(os.Stderr, "worker scan error:", err)
			return
		}
		result, err := service.RunIngestQueue(service.RunIngestQueueOptions{
			ProjectPath:   opts.ProjectPath,
			Validator:     compilerQueueValidator(opts.Provider),
			SkipUnchanged: opts.SkipUnchanged,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker queue error:", err)
			return
		}
		if result.Processed > 0 {
			if err := service.RefreshRelationsArtifact(opts.ProjectPath); err != nil {
				fmt.Fprintln(os.Stderr, "worker relations error:", err)
			}
			if err := syncWrittenWikiPages(ctx, opts.ProjectPath, opts.DBDSN, opts.ProjectID, []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}); err != nil {
				fmt.Fprintln(os.Stderr, "worker sync error:", err)
			}
		}
	}
	run()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	path := fs.String("path", runtimeConfig.Project.Path, "project path")
	name := fs.String("name", runtimeConfig.Project.Name, "project name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := wiki.InitProject(wiki.ProjectOptions{Path: *path, Name: *name}); err != nil {
		return err
	}
	fmt.Println("initialized", *path)
	return nil
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	source := fs.String("source", "", "source file")
	title := fs.String("title", "", "source title")
	kind := fs.String("kind", "source", "source kind")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for wiki sync")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := service.IngestSource(service.IngestOptions{
		ProjectPath: *project,
		SourcePath:  *source,
		Title:       *title,
		Kind:        *kind,
	})
	if err != nil {
		return err
	}
	fmt.Printf("raw=%s\nwiki=%s\nsha256=%s\n", result.RawPath, result.WikiPath, result.SHA256)
	if err := syncWrittenWikiPages(context.Background(), *project, *dbDSN, *projectIDFlag, aggregateWikiPaths(result.WikiPath)); err != nil {
		return err
	}
	return nil
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	q := fs.String("q", "", "query")
	limit := fs.Int("limit", 10, "max results")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "query agent: llm, mock")
	showProgress := fs.Bool("progress", true, "print live query progress to stderr")
	saveTitle := fs.String("save-title", "", "write answer back to wiki/syntheses with this title")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for graph evidence")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var progressReporter *queryProgressReporter
	var progress service.QueryProgressFunc
	if *showProgress {
		progressReporter = newQueryProgressReporter(os.Stderr, queryProgressHeartbeatInterval)
		defer progressReporter.Stop()
		progress = progressReporter.Progress
		progress(service.QueryProgressEvent{Type: "started", Message: "查询命令已启动"})
	}
	var agent service.QueryAgent
	switch *agentName {
	case "llm":
		configuredAgent, err := service.NewQueryAgent(runtimeConfig.LLM)
		if err != nil {
			return err
		}
		agent = configuredAgent
	case "mock":
		agent = service.MockQueryAgent{}
	default:
		return fmt.Errorf("unknown query agent %q", *agentName)
	}
	ctx := context.Background()
	dsn := *dbDSN
	projectID := *projectIDFlag
	dbHandle, err := openDBStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer dbHandle.Close()
	var graphStore service.GraphEvidenceStore
	var searchStore service.SearchEvidenceStore
	var queryLogStore service.QueryLogStore
	var embeddingProvider service.EmbeddingProvider
	if dbHandle != nil {
		projectID, err = requireProjectID(projectID)
		if err != nil {
			return err
		}
		graphStore = dbHandle.store
		searchStore = dbHandle.store
		queryLogStore = dbHandle.store
		configuredEmbedding, ok, err := service.NewEmbeddingProvider(runtimeConfig.Embedding)
		if err != nil {
			return err
		}
		if ok {
			embeddingProvider = configuredEmbedding
		}
	}
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		ProjectPath:       *project,
		ProjectID:         projectID,
		Question:          *q,
		Limit:             *limit,
		Agent:             agent,
		SearchStore:       searchStore,
		GraphStore:        graphStore,
		QueryLogStore:     queryLogStore,
		EmbeddingProvider: embeddingProvider,
		Context:           ctx,
		Progress:          progress,
		Runtime:           queryRuntimeOptions(runtimeConfig.Query),
	})
	if progressReporter != nil {
		progressReporter.Stop()
	}
	if err != nil {
		return err
	}
	if *saveTitle != "" && !answer.Plan.CanWriteBack {
		return fmt.Errorf("query answer is not eligible for writeback; use --agent llm for LLM Wiki synthesis")
	}
	fmt.Printf("intent=%s\nmode=%s\nwriteback=%t\n", answer.Plan.Intent, answer.Plan.AnswerMode, answer.Plan.CanWriteBack)
	if answer.Status != "" {
		fmt.Printf("status=%s\n", answer.Status)
	}
	if answer.Candidate != "" {
		fmt.Printf("candidate=%s\n", answer.Candidate)
	}
	if answer.Plan.ReasoningMode != "" {
		fmt.Printf("reasoning_mode=%s\n", answer.Plan.ReasoningMode)
	}
	if len(answer.Plan.Requirements) > 0 {
		fmt.Println("requirements:")
		for _, requirement := range answer.Plan.Requirements {
			fmt.Printf("- %s. %s [%s]\n", requirement.ID, requirement.Text, requirement.Kind)
		}
	}
	if len(answer.Plan.Hypotheses) > 0 {
		fmt.Println("hypotheses:")
		for _, hypothesis := range answer.Plan.Hypotheses {
			fmt.Printf("- %s: %s\n", hypothesis.Candidate, hypothesis.Rationale)
		}
	}
	if answer.SuggestedWritebackTitle != "" {
		fmt.Printf("suggested_writeback_title=%s\n", answer.SuggestedWritebackTitle)
	}
	fmt.Println("read-first:")
	for _, rel := range answer.Plan.ReadFirst {
		fmt.Printf("- %s\n", rel)
	}
	fmt.Println("search-plan:")
	for _, search := range answer.Plan.Searches {
		fmt.Printf("- %q weight=%d reason=%s\n", search.Text, search.Weight, search.Rationale)
	}
	if len(answer.Trace) > 0 {
		fmt.Println("trace:")
		for _, step := range answer.Trace {
			action := step.Action.Action
			target := step.Action.Path
			if target == "" {
				target = step.Action.Query
			}
			if target == "" {
				target = step.Action.Rationale
			}
			fmt.Printf("- step=%d action=%s target=%q observation=%s\n", step.Step, action, target, step.Observation)
		}
	}
	if len(answer.Verification) > 0 {
		fmt.Println("verification:")
		for _, verification := range answer.Verification {
			fmt.Printf("- pass=%d kind=%s accepted=%t summary=%s\n", verification.Pass, verification.Kind, verification.Accepted, verification.Summary)
		}
	}
	if len(answer.EvidenceChecks) > 0 {
		fmt.Println("evidence-checks:")
		for _, check := range answer.EvidenceChecks {
			fmt.Printf("- requirement=%s status=%s evidence=%s explanation=%s\n", check.RequirementID, check.Status, strings.Join(check.EvidencePaths, ","), check.Explanation)
		}
	}
	if answer.IncompleteReason != "" {
		fmt.Printf("incomplete_reason=%s\n", answer.IncompleteReason)
	}
	fmt.Println("\nanswer:")
	fmt.Println(answer.Answer)
	if len(answer.Citations) > 0 {
		fmt.Println("\ncitations:")
		for _, citation := range answer.Citations {
			fmt.Printf("- %s [%s] (%s)\n", citation.Title, citation.Kind, citation.Path)
		}
	}
	fmt.Println("\nresults:")
	for i, result := range answer.Results {
		fmt.Printf("%d. %s [%s] (%s) score=%d\n%s\n\n", i+1, result.Title, result.Kind, result.Path, result.Score, result.Snippet)
	}
	for _, note := range answer.Notes {
		fmt.Printf("note: %s\n", note)
	}
	if *saveTitle != "" {
		writebackTitle := *saveTitle
		if writebackTitle == "auto" {
			writebackTitle = answer.SuggestedWritebackTitle
		}
		writeback, err := service.WriteQueryAnswer(service.QueryWritebackOptions{
			ProjectPath: *project,
			Title:       writebackTitle,
			Answer:      answer,
		})
		if err != nil {
			return err
		}
		fmt.Printf("saved=%s\n", writeback.Path)
		if err := syncWrittenWikiPages(ctx, *project, dsn, projectID, aggregateWikiPaths(writeback.Path)); err != nil {
			return err
		}
	}
	return nil
}

func runLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	agentName := fs.String("agent", "structural", "lint agent: structural, llm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	issues, err := service.LintWiki(*project)
	if err != nil {
		return err
	}
	switch *agentName {
	case "structural":
	case "llm":
		agent, err := requireWikiReviewAgent("lint --agent llm")
		if err != nil {
			return err
		}
		reviewIssues, err := service.ReviewWiki(service.WikiReviewOptions{
			ProjectPath: *project,
			Agent:       agent,
		})
		if err != nil {
			return err
		}
		issues = append(issues, reviewIssues...)
	default:
		return fmt.Errorf("unknown lint agent %q", *agentName)
	}
	printLintIssues(issues)
	return nil
}

func requireWikiReviewAgent(command string) (service.WikiReviewAgent, error) {
	agent, err := service.NewWikiReviewAgent(runtimeConfig.LLM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	return agent, nil
}

func runReviewWiki(args []string) error {
	fs := flag.NewFlagSet("review-wiki", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	agentName := fs.String("agent", runtimeConfig.Server.Agent, "review agent: llm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var agent service.WikiReviewAgent
	switch *agentName {
	case "llm":
		envAgent, err := requireWikiReviewAgent("review-wiki")
		if err != nil {
			return err
		}
		agent = envAgent
	default:
		return fmt.Errorf("unknown review agent %q", *agentName)
	}
	issues, err := service.ReviewWiki(service.WikiReviewOptions{
		ProjectPath: *project,
		Agent:       agent,
	})
	if err != nil {
		return err
	}
	printLintIssues(issues)
	return nil
}

func runReviewTasks(args []string) error {
	fs := flag.NewFlagSet("review-tasks", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	projectID := fs.String("project-id", runtimeConfig.Database.ProjectID, "project id used to calculate stable review ids")
	status := fs.String("status", "", "filter by status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	items, err := wiki.ScanReviewItems(wiki.ScanOptions{
		ProjectPath: *project,
		ProjectID:   *projectID,
	})
	if err != nil {
		return err
	}
	count := 0
	for _, item := range items {
		if *status != "" && item.Status != *status {
			continue
		}
		count++
		fmt.Printf("%s [%s] %s | %s\n", item.ID, item.Status, item.Type, item.Title)
		if item.Description != "" {
			fmt.Printf("  %s\n", firstLine(item.Description))
		}
		for _, page := range item.AffectedPages {
			fmt.Printf("  page=%s\n", page)
		}
	}
	fmt.Printf("reviews=%d\n", count)
	return nil
}

func runResolveReview(args []string) error {
	fs := flag.NewFlagSet("resolve-review", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	projectID := fs.String("project-id", runtimeConfig.Database.ProjectID, "project id used to calculate stable review ids")
	id := fs.String("id", "", "review id")
	status := fs.String("status", "resolved", "new status: resolved, dismissed, open")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("review id is required")
	}
	resolvedAt := time.Time{}
	if *status == "resolved" || *status == "dismissed" {
		resolvedAt = time.Now().UTC()
	}
	item, err := wiki.UpdateReviewItemStatus(*project, *projectID, *id, *status, resolvedAt)
	if err != nil {
		return err
	}
	fmt.Printf("review=%s\nstatus=%s\ntitle=%s\n", item.ID, item.Status, item.Title)
	return nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func printLintIssues(issues []service.LintIssue) {
	if len(issues) == 0 {
		fmt.Println("ok")
		return
	}
	for i, issue := range issues {
		fmt.Printf("%s. %s %s: %s\n", strconv.Itoa(i+1), issue.Type, issue.Path, issue.Detail)
	}
}

func runSyncWikiPG(args []string) error {
	fs := flag.NewFlagSet("sync-wiki-pg", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN")
	migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before sync")
	embed := fs.Bool("embed", false, "write page embeddings using the configured embedding provider")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	projectID, err := requireProjectID(*projectIDFlag)
	if err != nil {
		return err
	}
	dbHandle, err := openDBStore(ctx, *dbDSN)
	if err != nil {
		return err
	}
	if dbHandle == nil {
		return fmt.Errorf("database DSN is required; pass --db-dsn or configure database.dsn")
	}
	defer dbHandle.Close()
	if *migrateDB {
		if err := dbHandle.store.Migrate(ctx); err != nil {
			return err
		}
	}
	if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
		return err
	}
	var embeddingProvider service.EmbeddingProvider
	if *embed {
		configuredEmbedding, ok, err := service.NewEmbeddingProvider(runtimeConfig.Embedding)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("embedding sync requires embedding.model and embedding.api_key")
		}
		embeddingProvider = configuredEmbedding
	}
	result, err := service.SyncWikiPagesToStore(ctx, service.WikiSyncOptions{
		ProjectPath:       *project,
		ProjectID:         projectID,
		Store:             dbHandle.store,
		EmbeddingProvider: embeddingProvider,
	})
	if err != nil {
		return err
	}
	fmt.Printf("pages=%d\nversions=%d\nreviews=%d\nsources=%d\nsource_manifest_entries=%d\nembeddings=%d\n", result.Pages, result.Versions, result.Reviews, result.Sources, result.SourceManifestEntries, result.Embeddings)
	return nil
}

func syncWrittenWikiPages(ctx context.Context, projectPath, dsn, projectID string, paths []string) error {
	if dsn == "" || len(paths) == 0 {
		return nil
	}
	projectID, err := requireProjectID(projectID)
	if err != nil {
		return err
	}
	dbHandle, err := openDBStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer dbHandle.Close()
	if err := ensurePGProject(ctx, dbHandle.store, projectID, projectPath); err != nil {
		return err
	}
	var embeddingProvider service.EmbeddingProvider
	configuredEmbedding, ok, err := service.NewEmbeddingProvider(runtimeConfig.Embedding)
	if err != nil {
		return err
	}
	if ok {
		embeddingProvider = configuredEmbedding
	}
	result, err := service.SyncWikiPagePathsToStore(ctx, service.WikiSyncOptions{
		ProjectPath:       projectPath,
		ProjectID:         projectID,
		Store:             dbHandle.store,
		EmbeddingProvider: embeddingProvider,
	}, paths)
	if err != nil {
		return err
	}
	sourceManifestEntries, err := service.SyncSourceManifestToStore(ctx, service.WikiSyncOptions{
		ProjectPath: projectPath,
		ProjectID:   projectID,
		Store:       dbHandle.store,
	})
	if err != nil {
		return err
	}
	if result.Pages > 0 || result.Reviews > 0 || sourceManifestEntries > 0 || result.Embeddings > 0 {
		fmt.Printf("pg_pages=%d\npg_reviews=%d\npg_sources=%d\npg_source_manifest_entries=%d\npg_embeddings=%d\n", result.Pages, result.Reviews, sourceManifestEntries, sourceManifestEntries, result.Embeddings)
	}
	return nil
}

func aggregateWikiPaths(paths ...string) []string {
	out := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			out = append(out, filepath.ToSlash(path))
		}
	}
	return out
}

func batchWrittenWikiPaths(result compiler.BatchValidateResult) []string {
	paths := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}
	for _, item := range result.Results {
		if item.Skipped {
			continue
		}
		paths = append(paths, item.Files...)
	}
	return paths
}

func runCodeIndexGo(args []string) error {
	fs := flag.NewFlagSet("code-index-go", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	repoPath := fs.String("repo-path", "", "local Go repository path")
	repoID := fs.String("repo-id", "", "stable repository id")
	commit := fs.String("commit", "", "commit metadata override")
	includeTests := fs.Bool("include-tests", false, "include *_test.go files")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before graph sync")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := service.IndexGoCodeSnapshot(service.GoCodeIndexOptions{
		ProjectPath: *project, RepoID: *repoID, RepoPath: *repoPath, Commit: *commit, IncludeTests: *includeTests,
	})
	if err != nil {
		return err
	}
	fmt.Printf("repo=%s\ncommit=%s\ndirty=%t\nsource_sha256=%s\nsnapshot=%s\nreport=%s\noverview=%s\nnodes=%d\nedges=%d\ncommunities=%d\nprocesses=%d\n", result.RepoID, result.Commit, result.Dirty, result.SourceSHA256, result.SnapshotPath, result.ReportPath, result.OverviewPath, result.Nodes, result.Edges, result.Communities, result.Processes)
	if strings.TrimSpace(*dbDSN) != "" {
		ctx := context.Background()
		projectID, err := requireProjectID(*projectIDFlag)
		if err != nil {
			return err
		}
		dbHandle, err := openDBStore(ctx, *dbDSN)
		if err != nil {
			return err
		}
		defer dbHandle.Close()
		if *migrateDB {
			if err := dbHandle.store.Migrate(ctx); err != nil {
				return fmt.Errorf("migrate PostgreSQL before graph sync: %w", err)
			}
		}
		if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
			return err
		}
		if _, err := service.SyncCodeGraphSnapshot(ctx, dbHandle.store, projectID, result.Snapshot, result.SnapshotPath); err != nil {
			return fmt.Errorf("sync code graph to PostgreSQL (run with --migrate-db after upgrading): %w", err)
		}
		if _, err := service.SyncWikiPagePathsToStore(ctx, service.WikiSyncOptions{ProjectPath: *project, ProjectID: projectID, Store: dbHandle.store}, aggregateWikiPaths(result.WrittenPaths...)); err != nil {
			return err
		}
	}
	return nil
}

func runCodeImportGraphify(args []string) error {
	fs := flag.NewFlagSet("code-import-graphify", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	repoID := fs.String("repo-id", "", "repo id")
	repoPath := fs.String("repo-path", "", "repo path")
	graphPath := fs.String("graph", "", "graphify graph.json")
	reportPath := fs.String("report", "", "graphify GRAPH_REPORT.md")
	dbDSN := fs.String("db-dsn", runtimeConfig.Database.DSN, "PostgreSQL DSN for graph sync")
	projectIDFlag := fs.String("project-id", runtimeConfig.Database.ProjectID, "PostgreSQL project id")
	migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before graph sync")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := service.ImportGraphifyCodeSnapshot(service.CodeImportOptions{
		ProjectPath: *project,
		RepoID:      *repoID,
		RepoPath:    *repoPath,
		GraphPath:   *graphPath,
		ReportPath:  *reportPath,
	})
	if err != nil {
		return err
	}
	fmt.Printf("snapshot=%s\noverview=%s\nnodes=%d\nedges=%d\n", result.SnapshotDir, result.Overview, result.NodeCount, result.EdgeCount)
	ctx := context.Background()
	dsn := *dbDSN
	if dsn == "" {
		return nil
	}
	projectID, err := requireProjectID(*projectIDFlag)
	if err != nil {
		return err
	}
	dbHandle, err := openDBStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer dbHandle.Close()
	if *migrateDB {
		if err := dbHandle.store.Migrate(ctx); err != nil {
			return err
		}
	}
	if err := ensurePGProject(ctx, dbHandle.store, projectID, *project); err != nil {
		return err
	}
	effectiveRepoID := *repoID
	if effectiveRepoID == "" {
		effectiveRepoID = core.Slug(filepath.Base(*repoPath))
	}
	snap, err := codegraph.ImportGraphify(effectiveRepoID, *repoPath, *graphPath, *reportPath)
	if err != nil {
		return err
	}
	syncResult, err := service.SyncCodeGraphSnapshot(ctx, dbHandle.store, projectID, snap, filepath.ToSlash(filepath.Join(result.SnapshotDir, "graph.json")))
	if err != nil {
		return err
	}
	fmt.Printf("pg_repo=%s\npg_nodes=%d\npg_edges=%d\n", syncResult.RepoID, syncResult.Nodes, syncResult.Edges)
	var embeddingProvider service.EmbeddingProvider
	configuredEmbedding, ok, err := service.NewEmbeddingProvider(runtimeConfig.Embedding)
	if err != nil {
		return err
	}
	if ok {
		embeddingProvider = configuredEmbedding
	}
	wikiSync, err := service.SyncWikiPagePathsToStore(ctx, service.WikiSyncOptions{
		ProjectPath:       *project,
		ProjectID:         projectID,
		Store:             dbHandle.store,
		EmbeddingProvider: embeddingProvider,
	}, aggregateWikiPaths(result.Overview))
	if err != nil {
		return err
	}
	if wikiSync.Pages > 0 || wikiSync.Embeddings > 0 {
		fmt.Printf("pg_pages=%d\npg_embeddings=%d\n", wikiSync.Pages, wikiSync.Embeddings)
	}
	return nil
}

func usage() {
	fmt.Print(`kbcore - Go/PostgreSQL/Markdown knowledge core

Global:
  --config PATH   YAML config path (default: repository-root config.yaml)

Commands:
  init --path PATH [--name NAME]
  ingest --project PATH --source FILE [--title TITLE] [--kind KIND] [--db-dsn DSN --project-id ID]
  validate-llmwiki --project PATH --source FILE_OR_DIR [--title TITLE] [--agent llm|mock] [--skip-unchanged] [--progress] [--db-dsn DSN --project-id ID]
  audit-wiki --project PATH [--baseline PATH --report FILE]
  repair-wiki --project PATH [--apply]
  refresh-overview --project PATH [--agent llm|mock]
  queue-ingest --project PATH --source FILE [--title TITLE]
  scan-sources --project PATH
  source-layout migrate --project PATH [--dry-run|--apply] [--db-dsn DSN --project-id ID --migrate-db]
  source-layout status --project PATH
  run-queue --project PATH [--agent llm] [--max N] [--retry-failed] [--db-dsn DSN --project-id ID]
  maintain --project PATH [--agent llm] [--retry-failed] [--keep-done] [--review] [--sync-pg] [--db-dsn DSN --project-id ID]
  query --project PATH --q QUERY [--limit N] [--agent llm|mock] [--progress=true|false] [--save-title TITLE] [--db-dsn DSN --project-id ID]
  sync-wiki-pg --project PATH --db-dsn DSN --project-id ID [--migrate-db] [--embed]
  lint --project PATH [--agent structural|llm]
  review-wiki --project PATH [--agent llm]
  review-tasks --project PATH [--status open|resolved|dismissed]
  resolve-review --project PATH --id ID [--status resolved|dismissed|open]
  code-import-graphify --project PATH --repo-path PATH --graph graph.json [--repo-id ID] [--report GRAPH_REPORT.md] [--db-dsn DSN --project-id ID]
  code-index-go --project PATH --repo-path PATH [--repo-id ID] [--include-tests] [--db-dsn DSN --project-id ID --migrate-db]
  migrate-sql
  mcp --project PATH [--project-id ID]
  serve [--addr 127.0.0.1:19829] [--project PATH] [--agent llm] [--worker --scan-interval 30s] [--db-dsn DSN --project-id ID --migrate-db]
  wait-ready [--addr 127.0.0.1:19829] [--timeout 2h]
`)
}
