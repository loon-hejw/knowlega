package data

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ContextRequestRepository completes Node-created surface-context requests in
// the shared durable map. Request creation and pending reads remain in Node
// because its in-memory viewer-token sidecar is intentionally not persisted.
type ContextRequestRepository struct{ pg *Postgres }

func NewContextRequestRepository(pg *Postgres) *ContextRequestRepository {
	return &ContextRequestRepository{pg: pg}
}

func (r *ContextRequestRepository) Fulfill(ctx context.Context, id string, result json.RawMessage, failure *string) (bool, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('context_requests') IS NOT NULL").Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Node's DurableMap.merge bumps even when the request has expired, ensuring
	// every Node process invalidates its short-lived durable-map snapshot.
	if _, err := tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('context_requests',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`); err != nil {
		return false, err
	}
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM context_requests WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return false, nil
	}
	if failure != nil {
		status, err := json.Marshal("failed")
		if err != nil {
			return false, err
		}
		errorValue, err := json.Marshal(*failure)
		if err != nil {
			return false, err
		}
		document["status"], document["error"] = status, errorValue
	} else {
		status, err := json.Marshal("done")
		if err != nil {
			return false, err
		}
		document["status"], document["result"] = status, result
	}
	updated, err := json.Marshal(document)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE context_requests SET json=$2::jsonb WHERE id=$1", id, updated); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
