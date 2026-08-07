package data

import (
	"context"
	"time"
)

type ReplayRepository struct{ pg *Postgres }

func NewReplayRepository(pg *Postgres) *ReplayRepository { return &ReplayRepository{pg: pg} }

func (r *ReplayRepository) Claim(ctx context.Context, eventID string, expiresAt time.Time) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now()
	if _, err := tx.Exec(ctx, "DELETE FROM source_auth_replay WHERE expires_at < $1", now); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "INSERT INTO source_auth_replay(event_id,expires_at) VALUES($1,$2) ON CONFLICT(event_id) DO NOTHING", eventID, expiresAt)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
