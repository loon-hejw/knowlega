package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/api"
	"github.com/hejw/knowledge-core/internal/codegraph"
	"github.com/hejw/knowledge-core/internal/compiler"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/postgres"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

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
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "ingest":
		return runIngest(args[1:])
	case "validate-llmwiki":
		return runValidateLLMWiki(args[1:])
	case "queue-ingest":
		return runQueueIngest(args[1:])
	case "scan-sources":
		return runScanSources(args[1:])
	case "run-queue":
		return runRunQueue(args[1:])
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
	case "migrate-sql":
		fmt.Print(postgres.BootstrapSQL)
		return nil
	case "serve":
		return runServe(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runValidateLLMWiki(args []string) error {
	fs := flag.NewFlagSet("validate-llmwiki", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
	source := fs.String("source", "", "source file or directory")
	title := fs.String("title", "", "source title")
	agentName := fs.String("agent", "auto", "ingest agent: auto, mock, llm")
	skipUnchanged := fs.Bool("skip-unchanged", false, "skip sources whose content hash is unchanged in the source manifest")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for wiki sync; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider, err := ingestProvider(*agentName)
	if err != nil {
		return err
	}
	result, err := compiler.ValidateLLMWikiPath(compiler.ValidateOptions{
		ProjectPath:   *project,
		SourcePath:    *source,
		Title:         *title,
		Provider:      provider,
		SkipUnchanged: *skipUnchanged,
	})
	if err != nil {
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
	if err := syncWrittenWikiPages(context.Background(), *project, envOrValue(*dbDSN, "KB_CORE_DB_DSN"), envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID"), batchWrittenWikiPaths(result)); err != nil {
		return err
	}
	return nil
}

func ingestProvider(agentName string) (compiler.Provider, error) {
	switch agentName {
	case "auto":
		envProvider, ok, err := compiler.NewEnvProvider()
		if err != nil {
			return nil, err
		}
		if ok {
			return envProvider, nil
		}
		return compiler.MockProvider{}, nil
	case "mock":
		return compiler.MockProvider{}, nil
	case "llm":
		envProvider, ok, err := compiler.NewEnvProvider()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("llm agent requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL")
		}
		return envProvider, nil
	default:
		return nil, fmt.Errorf("unknown ingest agent %q", agentName)
	}
}

func runQueueIngest(args []string) error {
	fs := flag.NewFlagSet("queue-ingest", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
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
	project := fs.String("project", "", "project path")
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

func runRunQueue(args []string) error {
	fs := flag.NewFlagSet("run-queue", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
	agentName := fs.String("agent", "auto", "ingest agent: auto, mock, llm")
	maxTasks := fs.Int("max", 0, "maximum tasks to process; 0 means all")
	skipUnchanged := fs.Bool("skip-unchanged", true, "skip sources whose content hash is unchanged in the source manifest")
	retryFailed := fs.Bool("retry-failed", false, "retry failed tasks")
	keepDone := fs.Bool("keep-done", false, "keep completed tasks in .kbcore/ingest-queue.json")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for wiki sync; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
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
	fmt.Printf("processed=%d\ndone=%d\nfailed=%d\nskipped=%d\nfiles=%d\n", result.Processed, result.Done, result.Failed, result.Skipped, result.Files)
	for _, task := range result.Tasks {
		fmt.Printf("task=%s source=%s status=%s retries=%d\n", task.ID, task.SourcePath, task.Status, task.RetryCount)
		if task.Error != "" {
			fmt.Printf("error=%s\n", task.Error)
		}
	}
	if err := syncWrittenWikiPages(context.Background(), *project, envOrValue(*dbDSN, "KB_CORE_DB_DSN"), envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID"), []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md", "wiki/reviews.md"}); err != nil {
		return err
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
	addr := fs.String("addr", "127.0.0.1:19829", "listen address")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for graph evidence; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "default PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	dbHandle, err := openDBStore(ctx, envOrValue(*dbDSN, "KB_CORE_DB_DSN"))
	if err != nil {
		return err
	}
	defer dbHandle.Close()
	var graphStore service.GraphEvidenceStore
	var codeGraphStore service.CodeGraphStore
	var searchStore service.SearchEvidenceStore
	var wikiPageStore service.WikiPageStore
	var queryLogStore service.QueryLogStore
	var embeddingProvider service.EmbeddingProvider
	if dbHandle != nil {
		graphStore = dbHandle.store
		codeGraphStore = dbHandle.store
		searchStore = dbHandle.store
		wikiPageStore = dbHandle.store
		queryLogStore = dbHandle.store
		envEmbedding, ok, err := service.NewEnvEmbeddingProvider()
		if err != nil {
			return err
		}
		if ok {
			embeddingProvider = envEmbedding
		}
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServerWithOptions(api.ServerOptions{SearchStore: searchStore, GraphStore: graphStore, CodeGraphStore: codeGraphStore, WikiPageStore: wikiPageStore, QueryLogStore: queryLogStore, EmbeddingProvider: embeddingProvider, DefaultProjectID: envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID")}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Println("serving", *addr)
	return server.ListenAndServe()
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	path := fs.String("path", "", "project path")
	name := fs.String("name", "", "project name")
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
	project := fs.String("project", "", "project path")
	source := fs.String("source", "", "source file")
	title := fs.String("title", "", "source title")
	kind := fs.String("kind", "source", "source kind")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for wiki sync; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
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
	if err := syncWrittenWikiPages(context.Background(), *project, envOrValue(*dbDSN, "KB_CORE_DB_DSN"), envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID"), aggregateWikiPaths(result.WikiPath)); err != nil {
		return err
	}
	return nil
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
	q := fs.String("q", "", "query")
	limit := fs.Int("limit", 10, "max results")
	agentName := fs.String("agent", "auto", "query agent: auto, fallback, mock, llm")
	saveTitle := fs.String("save-title", "", "write answer back to wiki/syntheses with this title")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for graph evidence; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var agent service.QueryAgent
	switch *agentName {
	case "auto":
		envAgent, ok, err := service.NewEnvQueryAgent()
		if err != nil {
			return err
		}
		if ok {
			agent = envAgent
		}
	case "fallback":
		agent = service.FallbackQueryAgent{}
	case "mock":
		agent = service.MockQueryAgent{}
	case "llm":
		envAgent, ok, err := service.NewEnvQueryAgent()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("llm agent requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL")
		}
		agent = envAgent
	default:
		return fmt.Errorf("unknown query agent %q", *agentName)
	}
	ctx := context.Background()
	dsn := envOrValue(*dbDSN, "KB_CORE_DB_DSN")
	projectID := envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID")
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
		envEmbedding, ok, err := service.NewEnvEmbeddingProvider()
		if err != nil {
			return err
		}
		if ok {
			embeddingProvider = envEmbedding
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
	})
	if err != nil {
		return err
	}
	if *saveTitle != "" && !answer.Plan.CanWriteBack {
		return fmt.Errorf("query answer is not eligible for writeback; use --agent llm for LLM Wiki synthesis")
	}
	fmt.Printf("intent=%s\nmode=%s\nwriteback=%t\n", answer.Plan.Intent, answer.Plan.AnswerMode, answer.Plan.CanWriteBack)
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
	fmt.Println("\nanswer:")
	fmt.Println(answer.Answer)
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
	project := fs.String("project", "", "project path")
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
	envAgent, ok, err := service.NewEnvWikiReviewAgent()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s requires KB_CORE_LLM_API_KEY and KB_CORE_LLM_MODEL", command)
	}
	return envAgent, nil
}

func runReviewWiki(args []string) error {
	fs := flag.NewFlagSet("review-wiki", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
	agentName := fs.String("agent", "auto", "review agent: auto, llm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var agent service.WikiReviewAgent
	switch *agentName {
	case "auto", "llm":
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
	project := fs.String("project", "", "project path")
	projectID := fs.String("project-id", "local", "project id used to calculate stable review ids")
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
	project := fs.String("project", "", "project path")
	projectID := fs.String("project-id", "local", "project id used to calculate stable review ids")
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
	project := fs.String("project", "", "project path")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN; defaults to KB_CORE_DB_DSN")
	migrateDB := fs.Bool("migrate-db", false, "run PostgreSQL migrations before sync")
	embed := fs.Bool("embed", false, "write page embeddings using KB_CORE_EMBEDDING_* or OPENAI_* env")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	projectID, err := requireProjectID(envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID"))
	if err != nil {
		return err
	}
	dbHandle, err := openDBStore(ctx, envOrValue(*dbDSN, "KB_CORE_DB_DSN"))
	if err != nil {
		return err
	}
	if dbHandle == nil {
		return fmt.Errorf("database DSN is required; pass --db-dsn or set KB_CORE_DB_DSN")
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
		envEmbedding, ok, err := service.NewEnvEmbeddingProvider()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("embedding sync requires KB_CORE_EMBEDDING_MODEL and an API key")
		}
		embeddingProvider = envEmbedding
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
	envEmbedding, ok, err := service.NewEnvEmbeddingProvider()
	if err != nil {
		return err
	}
	if ok {
		embeddingProvider = envEmbedding
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
	out := []string{"wiki/index.md", "wiki/log.md", "wiki/overview.md"}
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

func runCodeImportGraphify(args []string) error {
	fs := flag.NewFlagSet("code-import-graphify", flag.ContinueOnError)
	project := fs.String("project", "", "project path")
	repoID := fs.String("repo-id", "", "repo id")
	repoPath := fs.String("repo-path", "", "repo path")
	graphPath := fs.String("graph", "", "graphify graph.json")
	reportPath := fs.String("report", "", "graphify GRAPH_REPORT.md")
	dbDSN := fs.String("db-dsn", "", "PostgreSQL DSN for graph sync; defaults to KB_CORE_DB_DSN")
	projectIDFlag := fs.String("project-id", "", "PostgreSQL project id; defaults to KB_CORE_PROJECT_ID")
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
	dsn := envOrValue(*dbDSN, "KB_CORE_DB_DSN")
	if dsn == "" {
		return nil
	}
	projectID, err := requireProjectID(envOrValue(*projectIDFlag, "KB_CORE_PROJECT_ID"))
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
	envEmbedding, ok, err := service.NewEnvEmbeddingProvider()
	if err != nil {
		return err
	}
	if ok {
		embeddingProvider = envEmbedding
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

Commands:
  init --path PATH [--name NAME]
  ingest --project PATH --source FILE [--title TITLE] [--kind KIND] [--db-dsn DSN --project-id ID]
  validate-llmwiki --project PATH --source FILE_OR_DIR [--title TITLE] [--agent auto|mock|llm] [--skip-unchanged] [--db-dsn DSN --project-id ID]
  queue-ingest --project PATH --source FILE [--title TITLE]
  scan-sources --project PATH
  run-queue --project PATH [--agent auto|mock|llm] [--max N] [--retry-failed] [--db-dsn DSN --project-id ID]
  query --project PATH --q QUERY [--limit N] [--agent auto|fallback|mock|llm] [--save-title TITLE] [--db-dsn DSN --project-id ID]
  sync-wiki-pg --project PATH --db-dsn DSN --project-id ID [--migrate-db] [--embed]
  lint --project PATH [--agent structural|llm]
  review-wiki --project PATH [--agent auto|llm]
  review-tasks --project PATH [--status open|resolved|dismissed]
  resolve-review --project PATH --id ID [--status resolved|dismissed|open]
  code-import-graphify --project PATH --repo-path PATH --graph graph.json [--repo-id ID] [--report GRAPH_REPORT.md] [--db-dsn DSN --project-id ID]
  migrate-sql
  serve [--addr 127.0.0.1:19829] [--db-dsn DSN --project-id ID]
`)
}
