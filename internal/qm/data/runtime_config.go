package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

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

func (r *RuntimeConfigRepository) SetSelection(ctx context.Context, scopeID, orgScopeID string, selection RuntimeSelection) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	orgSelection, err := r.selectionInTx(ctx, tx, "base_model_configs", orgScopeID)
	if err != nil {
		return err
	}

	var orgRevision int64
	if orgSelection != nil {
		orgRevision = orgSelection.Revision
	}

	var row RuntimeSelection
	if scopeID == orgScopeID {
		// org行: revision = orgRevision = prevOrgRevision + 1
		revision := orgRevision + 1
		row = RuntimeSelection{
			HarnessID:   selection.HarnessID,
			ModelID:     selection.ModelID,
			OrgRevision: revision,
			Revision:    revision,
			EffortLevel: selection.EffortLevel,
			FastMode:    selection.FastMode,
		}
	} else {
		// scope行: orgRevision = org当前revision, 不动revision
		current, err := r.selectionInTx(ctx, tx, "base_model_configs", scopeID)
		if err != nil {
			return err
		}
		revision := int64(0)
		if current != nil {
			revision = current.Revision
		}
		row = RuntimeSelection{
			HarnessID:   selection.HarnessID,
			ModelID:     selection.ModelID,
			OrgRevision: orgRevision,
			Revision:    revision,
			EffortLevel: selection.EffortLevel,
			FastMode:    selection.FastMode,
		}
	}

	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `INSERT INTO base_model_configs(id, json) VALUES($1, $2)
		ON CONFLICT(id) DO UPDATE SET json=$2`, scopeID, payload)
	if err != nil {
		return err
	}

	if err := bumpDurableMapVersion(ctx, tx, "base_model_configs"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *RuntimeConfigRepository) AcknowledgeSelection(ctx context.Context, scopeID, orgScopeID string) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	current, err := r.selectionInTx(ctx, tx, "base_model_configs", scopeID)
	if err != nil || current == nil {
		return err
	}

	orgSelection, err := r.selectionInTx(ctx, tx, "base_model_configs", orgScopeID)
	if err != nil {
		return err
	}
	orgRevision := int64(0)
	if orgSelection != nil {
		orgRevision = orgSelection.Revision
	}

	updated := *current
	updated.OrgRevision = orgRevision

	payload, err := json.Marshal(updated)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `UPDATE base_model_configs SET json=$1 WHERE id=$2`, payload, scopeID)
	if err != nil {
		return err
	}

	if err := bumpDurableMapVersion(ctx, tx, "base_model_configs"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *RuntimeConfigRepository) DeleteSelection(ctx context.Context, scopeID string) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `DELETE FROM base_model_configs WHERE id=$1`, scopeID)
	if err != nil {
		return err
	}

	if err := bumpDurableMapVersion(ctx, tx, "base_model_configs"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *RuntimeConfigRepository) selectionInTx(ctx context.Context, tx pgx.Tx, table, id string) (*RuntimeSelection, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, "SELECT json FROM "+table+" WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var selection RuntimeSelection
	if json.Unmarshal(raw, &selection) != nil || selection.HarnessID == "" || selection.ModelID == "" {
		return nil, nil
	}
	return &selection, nil
}

func (r *RuntimeConfigRepository) WebuiModels(ctx context.Context, scopeID string) ([]string, error) {
	raw, err := r.mapRow(ctx, "webui_model_configs", scopeID)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var value struct {
		Models []string `json:"models"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil
	}
	return value.Models, nil
}

func (r *RuntimeConfigRepository) ExternalSlackParticipants(ctx context.Context, scopeID string) (bool, error) {
	orgScopeID := extractOrgScope(scopeID)
	orgRaw, err := r.mapRow(ctx, "external_slack_participants_flag", orgScopeID)
	if err != nil {
		return false, err
	}
	scopeRaw, err := r.mapRow(ctx, "external_slack_participants_flag", scopeID)
	if err != nil {
		return false, err
	}
	var orgVal, scopeVal struct {
		Enabled bool `json:"enabled"`
	}
	if len(orgRaw) > 0 {
		_ = json.Unmarshal(orgRaw, &orgVal)
	}
	if len(scopeRaw) > 0 {
		_ = json.Unmarshal(scopeRaw, &scopeVal)
	}
	return orgVal.Enabled || scopeVal.Enabled, nil
}

type BrandingRaw struct {
	Accent    string `json:"accent"`
	Mark      string `json:"mark"`
	SelfLabel string `json:"selfLabel"`
}

func (r *RuntimeConfigRepository) Branding(ctx context.Context, scopeID string) (*BrandingRaw, error) {
	raw, err := r.mapRow(ctx, "branding_configs", scopeID)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var value BrandingRaw
	if json.Unmarshal(raw, &value) != nil {
		return nil, nil
	}
	return &value, nil
}

func extractOrgScope(scopeID string) string {
	parts := strings.SplitN(scopeID, ":", 2)
	if len(parts) != 2 {
		return scopeID
	}
	if parts[0] == "org" {
		return scopeID
	}
	if parts[0] == "personal" {
		return "org:default"
	}
	if parts[0] == "group" || parts[0] == "channel" {
		orgParts := strings.SplitN(parts[1], "_", 2)
		if len(orgParts) > 0 {
			return "org:" + orgParts[0]
		}
	}
	return "org:default"
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
