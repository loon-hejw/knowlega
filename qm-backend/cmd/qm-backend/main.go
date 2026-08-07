package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/go-kratos/kratos/v2"
	"github.com/hejw/qm-backend/internal/biz"
	"github.com/hejw/qm-backend/internal/config"
	"github.com/hejw/qm-backend/internal/data"
	"github.com/hejw/qm-backend/internal/knowledge"
	"github.com/hejw/qm-backend/internal/server"
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
	knowledgeEngine, err := knowledge.New(cfg.Knowledge.RootDir, data.NewKnowledgeScopeRepository(pg), data.NewKnowledgeQueryRepository(pg))
	if err != nil {
		slog.Error("build knowledge engine", "error", err)
		os.Exit(1)
	}
	logger := slog.Default()
	httpServer, err := server.NewHTTPServer(cfg, pg, knowledgeEngine, logger)
	if err != nil {
		slog.Error("build HTTP server", "error", err)
		os.Exit(1)
	}
	var queryAgent knowledge.QueryAgent
	var compilerAgent knowledge.CompilerAgent
	if cfg.Knowledge.LLM.BaseURL != "" || cfg.Knowledge.LLM.APIKey != "" || cfg.Knowledge.LLM.Model != "" {
		agent, agentErr := knowledge.NewOpenAICompatibleQueryAgent(cfg.Knowledge.LLM.BaseURL, cfg.Knowledge.LLM.APIKey, cfg.Knowledge.LLM.Model, time.Duration(cfg.Knowledge.LLM.TimeoutSeconds)*time.Second)
		if agentErr != nil {
			slog.Error("build knowledge LLM agent", "error", agentErr)
			os.Exit(1)
		}
		queryAgent = agent
		compilerAgent = agent
	}
	grpcServer := server.NewGRPCServer(cfg, projects, data.NewRunRepository(pg), data.NewCronRepository(pg), data.NewDeploymentLayerRepository(pg), knowledgeEngine, queryAgent, compilerAgent)
	app := kratos.New(kratos.Name("qm-backend"), kratos.Server(httpServer, grpcServer))
	if err := app.Run(); err != nil {
		slog.Error("run", "error", err)
		os.Exit(1)
	}
}
