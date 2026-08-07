package data

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

const deploymentLayerCurrentID = "current"

// DeploymentLayerRepository implements the narrow durable-map primitive used
// by the Node deployment-layer maintainer. It intentionally does not parse or
// approve a layer: Node's validation/materialization workflow remains the
// authority until it is moved as one atomic unit.
type DeploymentLayerRepository struct{ pg *Postgres }

func NewDeploymentLayerRepository(pg *Postgres) *DeploymentLayerRepository {
	return &DeploymentLayerRepository{pg: pg}
}

func (r *DeploymentLayerRepository) Get(ctx context.Context) (json.RawMessage, bool, error) {
	var payload json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM deployment_layer WHERE id=$1", deploymentLayerCurrentID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

// PutIfAbsent mirrors DurableMap.putIfAbsent: it returns an existing document
// unchanged, and it invalidates the shared map version even when the insert
// loses its conflict race.
func (r *DeploymentLayerRepository) PutIfAbsent(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(payload) {
		return nil, errors.New("deployment layer payload must be JSON")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bumpDurableMapVersion(ctx, tx, "deployment_layer"); err != nil {
		return nil, err
	}
	var stored json.RawMessage
	err = tx.QueryRow(ctx, `INSERT INTO deployment_layer(id,json) VALUES($1,$2::jsonb)
ON CONFLICT(id) DO UPDATE SET json=deployment_layer.json RETURNING json`, deploymentLayerCurrentID, string(payload)).Scan(&stored)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return stored, nil
}

// CompareAndSet is the DurableMap.update primitive expressed as an optimistic
// compare-and-swap. JSONB equality deliberately ignores insignificant object
// key order, matching PostgreSQL DurableMap semantics.
func (r *DeploymentLayerRepository) CompareAndSet(ctx context.Context, expected, next json.RawMessage) (payload json.RawMessage, found, updated bool, err error) {
	if !json.Valid(expected) || !json.Valid(next) {
		return nil, false, false, errors.New("deployment layer payload must be JSON")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = bumpDurableMapVersion(ctx, tx, "deployment_layer"); err != nil {
		return nil, false, false, err
	}
	err = tx.QueryRow(ctx, "SELECT json FROM deployment_layer WHERE id=$1 FOR UPDATE", deploymentLayerCurrentID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.Commit(ctx); err != nil {
			return nil, false, false, err
		}
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	var matches bool
	if err = tx.QueryRow(ctx, "SELECT $1::jsonb = $2::jsonb", string(payload), string(expected)).Scan(&matches); err != nil {
		return nil, false, false, err
	}
	if !matches {
		if err = tx.Commit(ctx); err != nil {
			return nil, false, false, err
		}
		return payload, true, false, nil
	}
	if _, err = tx.Exec(ctx, "UPDATE deployment_layer SET json=$2::jsonb WHERE id=$1", deploymentLayerCurrentID, string(next)); err != nil {
		return nil, false, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, false, err
	}
	return next, true, true, nil
}
