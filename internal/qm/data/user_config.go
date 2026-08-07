package data

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
)

// UserConfigSnapshot is the safe, public subset of Node's scope configuration
// used by the admin user detail view. Connector secrets are never read or
// returned; only the same public metadata Node exposes is projected.
type UserConfigSnapshot struct {
	SecurityPosture string          `json:"securityPosture"`
	CommandPolicy   json.RawMessage `json:"commandPolicy"`
	Egress          json.RawMessage `json:"egress"`
	BaseModel       *string         `json:"baseModel"`
	Connectors      []any           `json:"connectors"`
}

type UserConfigRepository struct{ pg *Postgres }

func NewUserConfigRepository(pg *Postgres) *UserConfigRepository {
	return &UserConfigRepository{pg: pg}
}

func (r *UserConfigRepository) Snapshot(ctx context.Context, orgScopeID, scopeID string) (UserConfigSnapshot, error) {
	orgPosture, err := r.securityPosture(ctx, orgScopeID)
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	scopePosture, err := r.securityPosture(ctx, scopeID)
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	policy, err := r.scopedValue(ctx, "command_policies", scopeID, "policy")
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	egress, err := r.scopedValue(ctx, "egress_policies", scopeID, "policy")
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	model, err := r.baseModel(ctx, scopeID)
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	connectors, err := r.connectors(ctx, scopeID)
	if err != nil {
		return UserConfigSnapshot{}, err
	}
	return UserConfigSnapshot{
		SecurityPosture: composeSecurityPosture(orgPosture, scopePosture),
		CommandPolicy:   policy,
		Egress:          egress,
		BaseModel:       model,
		Connectors:      connectors,
	}, nil
}

func (r *UserConfigRepository) securityPosture(ctx context.Context, scopeID string) (string, error) {
	raw, err := r.scopedValue(ctx, "security_postures", scopeID, "posture")
	if err != nil || len(raw) == 0 {
		return "", err
	}
	var posture string
	if json.Unmarshal(raw, &posture) != nil || !validSecurityPosture(posture) {
		return "", nil
	}
	return posture, nil
}

func (r *UserConfigRepository) baseModel(ctx context.Context, scopeID string) (*string, error) {
	raw, err := r.scopedValue(ctx, "base_model_configs", scopeID, "modelId")
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var model string
	if json.Unmarshal(raw, &model) != nil || model == "" {
		return nil, nil
	}
	return &model, nil
}

func (r *UserConfigRepository) scopedValue(ctx context.Context, table, scopeID, key string) (json.RawMessage, error) {
	exists, err := r.tableExists(ctx, table)
	if err != nil || !exists {
		return nil, err
	}
	var raw json.RawMessage
	err = r.pg.Pool.QueryRow(ctx, "SELECT json FROM "+table+" WHERE id=$1", scopeID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return nil, nil
	}
	value := document[key]
	if len(value) == 0 || string(value) == "null" {
		return nil, nil
	}
	return append(json.RawMessage(nil), value...), nil
}

func (r *UserConfigRepository) connectors(ctx context.Context, scopeID string) ([]any, error) {
	exists, err := r.tableExists(ctx, "connector_clients")
	if err != nil || !exists {
		return []any{}, err
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT json FROM connector_clients ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record struct {
			ScopeID           string   `json:"scopeId"`
			Provider          string   `json:"provider"`
			ClientID          string   `json:"clientId"`
			SecretEnc         string   `json:"secretEnc"`
			Scopes            []string `json:"scopes"`
			RedirectAllowlist []string `json:"redirectAllowlist"`
			ConsentMode       string   `json:"consentMode"`
			HostedDomain      string   `json:"hostedDomain"`
			Enabled           bool     `json:"enabled"`
			UpdatedBy         string   `json:"updatedBy"`
			UpdatedAt         int64    `json:"updatedAt"`
		}
		if json.Unmarshal(raw, &record) != nil || record.ScopeID != scopeID || record.Provider == "" || record.ClientID == "" {
			continue
		}
		item := map[string]any{"provider": record.Provider, "clientId": record.ClientID, "enabled": record.Enabled, "hasSecret": record.SecretEnc != "", "updatedAt": record.UpdatedAt}
		if record.Scopes != nil {
			item["scopes"] = record.Scopes
		}
		if record.RedirectAllowlist != nil {
			item["redirectAllowlist"] = record.RedirectAllowlist
		}
		if record.ConsentMode != "" {
			item["consentMode"] = record.ConsentMode
		}
		if record.HostedDomain != "" {
			item["hostedDomain"] = record.HostedDomain
		}
		if record.UpdatedBy != "" {
			item["updatedBy"] = record.UpdatedBy
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["provider"].(string) < items[j]["provider"].(string) })
	result := make([]any, len(items))
	for i := range items {
		result[i] = items[i]
	}
	return result, nil
}

func (r *UserConfigRepository) tableExists(ctx context.Context, table string) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	return exists, err
}

func validSecurityPosture(value string) bool {
	return value == "dangerous" || value == "auto" || value == "strict"
}

func composeSecurityPosture(org, scope string) string {
	rank := map[string]int{"dangerous": 0, "auto": 1, "strict": 2}
	if !validSecurityPosture(org) {
		org = "auto"
	}
	if validSecurityPosture(scope) && rank[scope] > rank[org] {
		return scope
	}
	return org
}
