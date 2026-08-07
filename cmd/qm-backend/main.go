package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	agentcompiler "github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	agentconfig "github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	knowlegapostgres "github.com/loon-hejw/knowlega/internal/agent/knowlega/postgres"
	agentservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"github.com/loon-hejw/knowlega/internal/qm/server"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to YAML configuration")
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
	projects := biz.NewProjectUsecase(data.NewProjectRepository(pg, cfg.QM.OrgID))
	scopeRepo := data.NewKnowledgeScopeRepository(pg)
	knowledgeStore, closeKnowledgeStore, err := openKnowledgeStore(ctx, pg, cfg.Database.URL)
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
	logger := slog.Default()
	httpServer, err := server.NewHTTPServer(cfg, pg, fullAgent, logger)
	if err != nil {
		slog.Error("build HTTP server", "error", err)
		os.Exit(1)
	}
	// The compatibility gRPC service now delegates to the internal Agent through
	// the engine. Keep its legacy extension points nil so no simplified engine
	// can accidentally become the production path.
	grpcServer := server.NewGRPCServer(cfg, projects, data.NewRunRepository(pg), data.NewCronRepository(pg), data.NewDeploymentLayerRepository(pg))
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	if cfg.Knowledge.Worker {
		go runKnowledgeWorker(workerCtx, cfg, scopeRepo, fullAgent)
	}
	app := kratos.New(kratos.Name("qm-backend"), kratos.Server(httpServer, grpcServer))
	if err := app.Run(); err != nil {
		slog.Error("run", "error", err)
		os.Exit(1)
	}
}

func runKnowledgeWorker(ctx context.Context, cfg config.Config, scopes *data.KnowledgeScopeRepository, agent *knowlega.Agent) {
	interval := time.Duration(cfg.Knowledge.ScanIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	run := func() {
		items, err := scopes.List(ctx, cfg.QM.OrgID)
		if err != nil {
			slog.Error("knowledge worker list scopes", "error", err)
			return
		}
		runReview := cfg.Knowledge.LLM.APIKey != "" && cfg.Knowledge.LLM.Model != ""
		for _, item := range items {
			_, err := agent.Maintain(ctx, knowlega.ScopeRef{OrgID: item.OrgID, ExternalScopeID: item.ExternalScopeID, Kind: item.Kind, Name: item.ProjectName}, runReview)
			if err != nil {
				slog.Error("knowledge worker maintain", "scope", item.ExternalScopeID, "kind", item.Kind, "error", err)
			}
		}
	}
	run()
	ticker := time.NewTicker(interval)
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

func buildKnowledgeAgent(cfg config.Config, store *knowlegapostgres.Store) (*knowlega.Agent, error) {
	agentCfg := agentconfig.Defaults()
	if cfg.Knowledge.LLM.BaseURL != "" {
		agentCfg.LLM.BaseURL = cfg.Knowledge.LLM.BaseURL
	}
	if cfg.Knowledge.LLM.APIKey != "" {
		agentCfg.LLM.APIKey = cfg.Knowledge.LLM.APIKey
	}
	if cfg.Knowledge.LLM.Model != "" {
		agentCfg.LLM.Model = cfg.Knowledge.LLM.Model
	}
	if cfg.Knowledge.LLM.TimeoutSeconds > 0 {
		agentCfg.LLM.Timeout.Duration = time.Duration(cfg.Knowledge.LLM.TimeoutSeconds) * time.Second
		agentCfg.LLM.OperationTimeout.Duration = agentCfg.LLM.Timeout.Duration
	}
	var queryAgent agentservice.QueryAgent
	var provider agentcompiler.Provider
	var reviewAgent agentservice.WikiReviewAgent
	configured := cfg.Knowledge.LLM.APIKey != "" || cfg.Knowledge.LLM.Model != "" || cfg.Knowledge.LLM.BaseURL != ""
	if configured {
		var err error
		provider, err = agentcompiler.NewProvider(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
		queryAgent, err = agentservice.NewQueryAgent(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
		reviewAgent, err = agentservice.NewWikiReviewAgent(agentCfg.LLM)
		if err != nil {
			return nil, err
		}
	}
	return knowlega.New(knowlega.AgentOptions{
		RootDir:       cfg.Knowledge.RootDir,
		QueryAgent:    queryAgent,
		Compiler:      provider,
		ReviewAgent:   reviewAgent,
		WikiStore:     store,
		SearchStore:   store,
		GraphStore:    store,
		QueryLogStore: store,
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

func openKnowledgeStore(ctx context.Context, pg *data.Postgres, dsn string) (*knowlegapostgres.Store, func(), error) {
	if _, err := pg.Pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS knowledge_core"); err != nil {
		return nil, func() {}, err
	}
	knowledgeDSN := withKnowledgeSearchPath(dsn)
	db, err := sql.Open("pgx", knowledgeDSN)
	if err != nil {
		return nil, func() {}, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, func() {}, err
	}
	store := knowlegapostgres.NewStore(db)
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, func() {}, err
	}
	return store, func() { _ = db.Close() }, nil
}

func withKnowledgeSearchPath(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err == nil && parsed.Scheme != "" {
		query := parsed.Query()
		query.Set("search_path", "knowledge_core")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=knowledge_core"
}
