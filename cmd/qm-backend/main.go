package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	agentcompiler "github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	agentconfig "github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	knowlegapostgres "github.com/loon-hejw/knowlega/internal/agent/knowlega/postgres"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	qmagent "github.com/loon-hejw/knowlega/internal/qm/agent"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/server"
	qmworker "github.com/loon-hejw/knowlega/internal/qm/worker"
)

func main() {
	configPath := flag.String("config", "configs/qm-config.yaml", "path to QM backend YAML configuration")
	legacyKnowledgePath := flag.String("import-legacy-knowledge", "", "import an existing Markdown knowledge project into a QM project and exit")
	legacyProjectName := flag.String("import-project-name", "", "QM project name for --import-legacy-knowledge")
	legacyProjectOwner := flag.String("import-project-owner", "", "QM principal id that owns the imported project")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, cfg.Database.URL)
	if err != nil {
		slog.Error("connect PostgreSQL", "error", err)
		os.Exit(1)
	}
	defer pg.Close()
	if err := pg.Migrate(ctx); err != nil {
		slog.Error("migrate PostgreSQL", "error", err)
		os.Exit(1)
	}
	scopeRepo := data.NewKnowledgeScopeRepository(pg)
	projectFiles := data.NewProjectFileMembershipRepository(pg)
	knowledgeStore, closeKnowledgeStore, err := openKnowledgeStore(ctx, cfg.Database.URL)
	if err != nil {
		slog.Error("build knowledge postgres store", "error", err)
		os.Exit(1)
	}
	defer closeKnowledgeStore()
	fullAgent, err := buildKnowledgeAgent(cfg, knowledgeStore)
	if err != nil {
		slog.Error("build knowledge agent", "error", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*legacyKnowledgePath) != "" {
		result, importErr := importLegacyKnowledgeProject(ctx, legacyKnowledgeImportOptions{
			Config: cfg, Postgres: pg, Agent: fullAgent, SourcePath: *legacyKnowledgePath,
			ProjectName: *legacyProjectName, OwnerID: *legacyProjectOwner,
		})
		if importErr != nil {
			slog.Error("import legacy knowledge project", "error", importErr)
			os.Exit(1)
		}
		slog.Info("legacy knowledge project imported", "project_id", result.ProjectID, "scope_id", result.ScopeID, "files", result.Files, "wiki_pages", result.WikiPages, "sources", result.Sources)
		return
	}
	logger := slog.Default()
	httpServer, err := server.NewHTTPServer(cfg, pg, fullAgent, logger)
	if err != nil {
		slog.Error("build HTTP server", "error", err)
		os.Exit(1)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	var workerWait sync.WaitGroup
	startWorker := func(run func(context.Context)) {
		workerWait.Add(1)
		go func() {
			defer workerWait.Done()
			run(workerCtx)
		}()
	}
	defer func() {
		cancelWorker()
		workerWait.Wait()
	}()
	runtimeTasks := data.NewRuntimeTaskRepository(pg)
	agentTasks := data.NewAgentTaskRepository(pg)
	runtimeTaskManager := qmworker.NewManager(runtimeTasks, time.Duration(cfg.Workers.ReapIntervalSeconds)*time.Second, logger)
	adapters := []qmagent.Adapter{}
	if _, configured := cfg.QM.Models.Harness("pi"); configured {
		piAdapter, err := qmagent.NewPiAdapter(cfg.QM.Models, nil)
		if err != nil {
			slog.Error("build pi harness", "error", err)
			os.Exit(1)
		}
		adapters = append(adapters, piAdapter)
	}
	if _, configured := cfg.QM.Models.Harness("opencode"); configured {
		openCodeAdapter, err := qmagent.NewOpenCodeAdapter(cfg.QM.Models, nil)
		if err != nil {
			slog.Error("build opencode harness", "error", err)
			os.Exit(1)
		}
		adapters = append(adapters, openCodeAdapter)
	}
	if _, configured := cfg.QM.Models.Harness("codex"); configured {
		codexAdapter, err := qmagent.NewCodexAdapter(cfg.QM.Models, nil)
		if err != nil {
			slog.Error("build codex harness", "error", err)
			os.Exit(1)
		}
		adapters = append(adapters, codexAdapter.WithTaskStore(agentTasks))
	}
	if _, configured := cfg.QM.Models.Harness("claude"); configured {
		claudeAdapter, err := qmagent.NewClaudeAdapter(cfg.QM.Models, nil)
		if err != nil {
			slog.Error("build claude harness", "error", err)
			os.Exit(1)
		}
		adapters = append(adapters, claudeAdapter.WithTaskStore(agentTasks))
	}
	if len(adapters) > 0 {
		var sandboxTasks qmagent.SandboxTaskStore
		if cfg.QM.SandboxDefaultBackend == "local" {
			sandboxTasks = runtimeTasks
			sandboxConfig := cfg.Workers.Pools["sandbox"]
			sandboxPool, poolErr := qmworker.NewPool(runtimeTasks, qmworker.PoolConfig{
				Name: "sandbox", Kinds: []string{qmagent.SandboxExecTaskKind}, Concurrency: sandboxConfig.Concurrency,
				LeaseTTL: time.Duration(sandboxConfig.LeaseTTLSeconds) * time.Second, HeartbeatInterval: time.Duration(sandboxConfig.HeartbeatIntervalSeconds) * time.Second,
				PollInterval: time.Duration(sandboxConfig.PollIntervalMillis) * time.Millisecond, MaxClaimBackoff: time.Duration(sandboxConfig.MaxClaimBackoffMillis) * time.Millisecond,
			}, (qmagent.LocalDockerSandbox{WorkspaceRoot: cfg.QM.AgentWorkspaceRoot}).Handler(), logger)
			if poolErr != nil {
				slog.Error("build sandbox worker", "error", poolErr)
				os.Exit(1)
			}
			runtimeTaskManager.Add(sandboxPool)
		}
		toolResolver, err := qmagent.NewCoreToolContextResolver(qmagent.CoreToolContextOptions{
			Memory: data.NewMemoryRepository(pg), Sessions: data.NewSessionRepository(pg),
			Knowledge: fullAgent, WorkspaceRoot: cfg.QM.AgentWorkspaceRoot, SandboxTasks: sandboxTasks,
		})
		if err != nil {
			slog.Error("build agent tool context", "error", err)
			os.Exit(1)
		}
		agentEngine, err := qmagent.NewEngine(
			cfg.QM.Models,
			data.NewHarnessSessionRepository(pg),
			adapters,
			qmagent.DurableChoiceResolver{Models: cfg.QM.Models, Store: data.NewRuntimeConfigRepository(pg)},
		)
		if err != nil {
			slog.Error("build agent engine", "error", err)
			os.Exit(1)
		}
		defer func() { _ = agentEngine.Close(context.Background()) }()
		turnConfig := cfg.Workers.Pools["turn"]
		approvalRepository := data.NewAgentApprovalRepository(pg)
		inboundMaterializer, err := qmagent.NewLocalInboundMaterializer(cfg.QM.AgentWorkspaceRoot, cfg.QM.FileStore.TransferLocalDir)
		if err != nil {
			slog.Error("build agent inbound materializer", "error", err)
			os.Exit(1)
		}
		bindings := qmagent.NewPostgresBindingsResolver(data.NewSessionRepository(pg), toolResolver, data.NewRunRepository(pg)).WithApprovals(approvalRepository, approvalRepository).WithInboundMaterializer(inboundMaterializer).WithTurnInputPreparer(qmagent.NewProjectKnowledgePreparer())
		turnPool, err := qmworker.NewPool(runtimeTasks, qmworker.PoolConfig{
			Name: "turn", Kinds: []string{qmagent.TurnTaskKind}, Concurrency: turnConfig.Concurrency,
			LeaseTTL:          time.Duration(turnConfig.LeaseTTLSeconds) * time.Second,
			HeartbeatInterval: time.Duration(turnConfig.HeartbeatIntervalSeconds) * time.Second,
			PollInterval:      time.Duration(turnConfig.PollIntervalMillis) * time.Millisecond,
			MaxClaimBackoff:   time.Duration(turnConfig.MaxClaimBackoffMillis) * time.Millisecond,
		}, qmagent.MirrorTurnRuns(qmagent.NewTurnTaskHandler(agentEngine, bindings), data.NewRunRepository(pg)), logger)
		if err != nil {
			slog.Error("build agent turn worker", "error", err)
			os.Exit(1)
		}
		runtimeTaskManager.Add(turnPool)
	}
	startWorker(runtimeTaskManager.Run)
	if cfg.Knowledge.Worker || cfg.Knowledge.AutoProcessProjectFiles {
		startWorker(func(ctx context.Context) {
			runKnowledgeWorker(ctx, cfg, scopeRepo, projectFiles, fullAgent)
		})
	}
	app := kratos.New(kratos.Name("qm-backend"), kratos.Server(httpServer))
	if err := app.Run(); err != nil {
		slog.Error("run", "error", err)
		os.Exit(1)
	}
}

func runKnowledgeWorker(ctx context.Context, cfg config.Config, scopes *data.KnowledgeScopeRepository, projectFiles *data.ProjectFileMembershipRepository, agent *knowlega.Agent) {
	interval := time.Duration(cfg.Knowledge.ScanIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	run := func(recovering bool) {
		items, err := scopes.List(ctx, cfg.QM.OrgID)
		if err != nil {
			slog.Error("knowledge worker list scopes", "error", err)
			return
		}
		baseModel := cfg.QM.Models.DefaultModel()
		provider, configured := cfg.QM.Models.ProviderForModel(baseModel)
		runReview := configured && provider.Protocol != "mock"
		for _, item := range items {
			projectScope := item.Kind == "project" && strings.HasPrefix(item.ExternalScopeID, "group:web-project-")
			if !shouldMaintainKnowledgeScope(cfg, item) {
				continue
			}
			ref := knowlega.ScopeRef{OrgID: item.OrgID, ExternalScopeID: item.ExternalScopeID, Kind: item.Kind, Name: item.ProjectName}
			if projectScope {
				_, err := server.ProcessProjectFileScope(ctx, strings.TrimPrefix(item.ExternalScopeID, "group:web-project-"), ref, projectFiles, scopes, agent, runReview, recovering)
				if err != nil {
					slog.Error("knowledge project file worker", "scope", item.ExternalScopeID, "error", err)
				}
				continue
			}
			if recovering {
				if _, recoverErr := agent.RecoverIngestQueue(ctx, ref); recoverErr != nil {
					slog.Error("knowledge worker recover queue", "scope", item.ExternalScopeID, "kind", item.Kind, "error", recoverErr)
				}
			}
			if _, err := agent.Maintain(ctx, ref, runReview); err != nil {
				slog.Error("knowledge worker maintain", "scope", item.ExternalScopeID, "kind", item.Kind, "error", err)
			}
			if _, err := server.RefreshKnowledgeScopeStatus(ctx, ref, scopes, agent); err != nil {
				slog.Error("knowledge worker refresh scope status", "scope", item.ExternalScopeID, "kind", item.Kind, "error", err)
			}
		}
	}
	run(true)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run(false)
		}
	}
}

func shouldMaintainKnowledgeScope(cfg config.Config, scope data.KnowledgeScope) bool {
	projectScope := scope.Kind == "project" && strings.HasPrefix(scope.ExternalScopeID, "group:web-project-")
	if projectScope {
		return cfg.Knowledge.AutoProcessProjectFiles
	}
	return cfg.Knowledge.Worker
}

func buildKnowledgeAgent(cfg config.Config, store *knowlegapostgres.Store) (*knowlega.Agent, error) {
	agentCfg := agentconfig.Defaults()
	baseModel := cfg.QM.Models.DefaultModel()
	modelProvider, modelConfigured := cfg.QM.Models.ProviderForModel(baseModel)
	if modelConfigured && modelProvider.Protocol != "mock" {
		agentCfg.LLM.Protocol = modelProvider.Protocol
		agentCfg.LLM.BaseURL = modelProvider.BaseURL
		agentCfg.LLM.APIKey = modelProvider.APIKey
		agentCfg.LLM.Model = baseModel
		agentCfg.LLM.UserAgent = modelProvider.UserAgent
		agentCfg.LLM.AnthropicVersion = modelProvider.AnthropicVersion
	}
	request := cfg.QM.Models.Request
	if request.TimeoutSeconds > 0 {
		agentCfg.LLM.Timeout.Duration = time.Duration(request.TimeoutSeconds) * time.Second
	}
	agentCfg.LLM.Retries = request.Retries
	if request.OperationTimeoutSeconds > 0 {
		agentCfg.LLM.OperationTimeout.Duration = time.Duration(request.OperationTimeoutSeconds) * time.Second
	} else {
		// The operation budget must outlive every per-attempt request deadline;
		// otherwise one slow request starves all configured retries.
		agentCfg.LLM.OperationTimeout.Duration = agentCfg.LLM.Timeout.Duration*time.Duration(request.Retries+1) + 5*time.Second*time.Duration(request.Retries)
	}
	if request.MaxInputChars > 0 {
		agentCfg.LLM.MaxInputChars = request.MaxInputChars
	}
	if request.MaxOutputTokens > 0 {
		agentCfg.LLM.MaxOutputTokens = request.MaxOutputTokens
	}
	agentCfg.LLM.DisableThinking = request.DisableThinking
	var provider agentcompiler.Provider
	var reviewAgent agentservice.WikiReviewAgent
	var maintenanceAgent agentservice.MaintenanceSynthesisAgent
	configured := modelConfigured && modelProvider.Protocol != "mock"
	if configured {
		var err error
		provider, err = agentcompiler.NewProvider(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
		reviewAgent, err = agentservice.NewWikiReviewAgent(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
		maintenanceAgent, err = agentservice.NewMaintenanceSynthesisAgent(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
	}
	return knowlega.New(knowlega.AgentOptions{
		RootDir:     cfg.Knowledge.RootDir,
		Compiler:    provider,
		ReviewAgent: reviewAgent,
		Maintenance: maintenanceAgent,
		WikiStore:   store,
		SearchStore: store,
		GraphStore:  store,
		AgentName: func() string {
			if configured {
				return "llm"
			}
			return "mock"
		}(),
		ProjectIDFor: func(ref knowlega.ScopeRef) string {
			sum := sha256.Sum256([]byte(ref.OrgID + "\x00" + ref.Kind + "\x00" + ref.ExternalScopeID))
			return hex.EncodeToString(sum[:16])
		},
	})
}

func openKnowledgeStore(ctx context.Context, dsn string) (*knowlegapostgres.Store, func(), error) {
	return knowlegapostgres.OpenStore(ctx, dsn)
}

func withKnowledgeSearchPath(dsn string) string {
	return knowlegapostgres.SearchPathDSN(dsn)
}
