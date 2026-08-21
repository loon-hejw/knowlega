package data

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type RuntimeSelection struct {
	HarnessID   string `json:"harnessId"`
	ModelID     string `json:"modelId"`
	OrgRevision int64  `json:"orgRevision,omitempty"`
	Revision    int64  `json:"revision,omitempty"`
	EffortLevel string `json:"effortLevel,omitempty"`
	FastMode    *bool  `json:"fastMode,omitempty"`
}

type RuntimeConfigRepository struct{ pg *Postgres }

func NewRuntimeConfigRepository(pg *Postgres) *RuntimeConfigRepository {
	return &RuntimeConfigRepository{pg: pg}
}

func (r *RuntimeConfigRepository) Selection(ctx context.Context, scopeID string) (*RuntimeSelection, error) {
	raw, err := r.mapRow(ctx, "base_model_configs", scopeID)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var selection RuntimeSelection
	if json.Unmarshal(raw, &selection) != nil || selection.HarnessID == "" || selection.ModelID == "" {
		return nil, nil
	}
	return &selection, nil
}

func (r *RuntimeConfigRepository) LegacyModel(ctx context.Context, scopeID string) (string, error) {
	raw, err := r.mapRow(ctx, "base_model_configs", scopeID)
	if err != nil || len(raw) == 0 {
		return "", err
	}
	var value struct {
		ModelID string `json:"modelId"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "", nil
	}
	return value.ModelID, nil
}

func (r *RuntimeConfigRepository) ApprovedHarnesses(ctx context.Context, orgScopeID string) ([]string, error) {
	raw, err := r.mapRow(ctx, "approved_harness_configs", orgScopeID)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var value struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil
	}
	return value.IDs, nil
}

func (r *RuntimeConfigRepository) mapRow(ctx context.Context, table, id string) (json.RawMessage, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM "+table+" WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}
