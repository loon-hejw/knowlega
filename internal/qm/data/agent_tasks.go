package data

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentTaskRepository persists the user-visible subagent lifecycle shared by
// the Node and Go harness implementations. It deliberately uses the existing
// tasks/task_events tables so the admin UI keeps one durable view during the
// migration.
type AgentTaskRepository struct{ pg *Postgres }

func NewAgentTaskRepository(pg *Postgres) *AgentTaskRepository {
	return &AgentTaskRepository{pg: pg}
}

func (r *AgentTaskRepository) CreateTask(ctx context.Context, id, sessionID, originRunID, title, status string) error {
	if r == nil || r.pg == nil || r.pg.Pool == nil {
		return errors.New("agent task repository is not configured")
	}
	id = strings.TrimSpace(id)
	sessionID = strings.TrimSpace(sessionID)
	originRunID = strings.TrimSpace(originRunID)
	title = strings.TrimSpace(title)
	if id == "" || sessionID == "" || originRunID == "" {
		return errors.New("agent task id, session id, and origin run id are required")
	}
	if title == "" {
		title = "subagent task"
	}
	if !validAgentTaskStatus(status) {
		return errors.New("invalid agent task status")
	}
	at := time.Now().UnixMilli()
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO tasks(id,session_id,origin_run_id,title,status,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$6)`, id, sessionID, originRunID, title, status, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,run_id,type,from_status,to_status,created_at)
VALUES($1,$2,'created',NULL,$3,$4)`, id, originRunID, status, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *AgentTaskRepository) TransitionTask(ctx context.Context, id, expected, next, runID string) (bool, error) {
	if r == nil || r.pg == nil || r.pg.Pool == nil {
		return false, errors.New("agent task repository is not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(runID) == "" || !validAgentTaskStatus(expected) || !validAgentTaskStatus(next) {
		return false, errors.New("agent task id, run id, and valid statuses are required")
	}
	at := time.Now().UnixMilli()
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var updated string
	if err := tx.QueryRow(ctx, `UPDATE tasks SET status=$3,updated_at=$4
WHERE id=$1 AND status=$2 RETURNING id`, id, expected, next, at).Scan(&updated); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,run_id,type,from_status,to_status,created_at)
VALUES($1,$2,'status_changed',$3,$4,$5)`, id, runID, expected, next, at); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func validAgentTaskStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "skipped", "failed":
		return true
	default:
		return false
	}
}
