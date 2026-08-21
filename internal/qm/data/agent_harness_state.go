package data

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type HarnessSessionRepository struct{ pg *Postgres }

func NewHarnessSessionRepository(pg *Postgres) *HarnessSessionRepository {
	return &HarnessSessionRepository{pg: pg}
}

func (r *HarnessSessionRepository) Select(ctx context.Context, sessionID, harnessID string) (string, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	harnessID = strings.TrimSpace(harnessID)
	if sessionID == "" || harnessID == "" {
		return "", false, errors.New("session id and harness id are required")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('agent-harness'),hashtext($1))", sessionID); err != nil {
		return "", false, err
	}
	var previous string
	err = tx.QueryRow(ctx, "SELECT harness_id FROM agent_harness_state WHERE session_id=$1 FOR UPDATE", sessionID).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	changed := previous != "" && previous != harnessID
	if _, err := tx.Exec(ctx, `INSERT INTO agent_harness_state(session_id,harness_id,updated_at) VALUES($1,$2,$3)
ON CONFLICT(session_id) DO UPDATE SET harness_id=EXCLUDED.harness_id,updated_at=EXCLUDED.updated_at`, sessionID, harnessID, time.Now().UnixMilli()); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return previous, changed, nil
}

func (r *HarnessSessionRepository) Delete(ctx context.Context, sessionID string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM agent_harness_state WHERE session_id=$1", sessionID)
	return err
}
