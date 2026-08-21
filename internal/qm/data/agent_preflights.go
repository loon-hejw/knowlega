package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type AgentPreflightRepository struct{ pg *Postgres }

func NewAgentPreflightRepository(pg *Postgres) *AgentPreflightRepository {
	return &AgentPreflightRepository{pg: pg}
}

func (r *AgentPreflightRepository) Load(ctx context.Context, runID string) (string, json.RawMessage, bool, error) {
	if r == nil || r.pg == nil || strings.TrimSpace(runID) == "" {
		return "", nil, false, nil
	}
	var route string
	var result json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT route,result FROM agent_preflights WHERE run_id=$1", runID).Scan(&route, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	return route, result, err == nil, err
}

func (r *AgentPreflightRepository) PutRoute(ctx context.Context, runID, route string) error {
	now := time.Now().UnixMilli()
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO agent_preflights(run_id,route,created_at,updated_at) VALUES($1,$2,$3,$3)
ON CONFLICT(run_id) DO UPDATE SET route=EXCLUDED.route,updated_at=EXCLUDED.updated_at`, runID, route, now)
	return err
}

func (r *AgentPreflightRepository) PutResult(ctx context.Context, runID, route string, result json.RawMessage) error {
	now := time.Now().UnixMilli()
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO agent_preflights(run_id,route,result,created_at,updated_at) VALUES($1,$2,$3,$4,$4)
ON CONFLICT(run_id) DO UPDATE SET route=EXCLUDED.route,result=EXCLUDED.result,updated_at=EXCLUDED.updated_at`, runID, route, result, now)
	return err
}
