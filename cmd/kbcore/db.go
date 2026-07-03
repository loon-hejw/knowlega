package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hejw/knowledge-core/internal/config"
	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/postgres"
)

type dbStoreHandle struct {
	db    *sql.DB
	store *postgres.Store
}

func openDBStore(ctx context.Context, dsn string) (*dbStoreHandle, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &dbStoreHandle{db: db, store: postgres.NewStore(db)}, nil
}

func (h *dbStoreHandle) Close() {
	if h != nil && h.db != nil {
		_ = h.db.Close()
	}
}

func envOrValue(value, envKey string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return config.Value(envKey)
}

func requireProjectID(projectID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", fmt.Errorf("project id is required when using PostgreSQL; pass --project-id or set KB_CORE_PROJECT_ID")
	}
	return projectID, nil
}

func ensurePGProject(ctx context.Context, store *postgres.Store, projectID, projectPath string) error {
	return store.UpsertProject(ctx, core.Project{
		ID:       projectID,
		Name:     projectID,
		RootPath: projectPath,
	})
}
