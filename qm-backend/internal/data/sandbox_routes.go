package data

import (
	"context"
	"encoding/json"
	"sort"
)

// SandboxRouteRepository reads Node's sandbox_routing durable map. Moving a
// route entails copying a workspace and managing sandboxes, so this repository
// intentionally supports only the metadata read projection.
type SandboxRouteRepository struct{ pg *Postgres }

type SandboxRoute struct {
	ScopeID          string   `json:"scopeId"`
	Backend          string   `json:"backend"`
	MigratedAt       string   `json:"migratedAt,omitempty"`
	MigrationSHA     string   `json:"migrationSha,omitempty"`
	CapabilitiesLost []string `json:"capabilitiesLost,omitempty"`
	Pinned           bool     `json:"pinned,omitempty"`
	Reason           string   `json:"reason,omitempty"`
}

func NewSandboxRouteRepository(pg *Postgres) *SandboxRouteRepository {
	return &SandboxRouteRepository{pg: pg}
}

func (r *SandboxRouteRepository) List(ctx context.Context) ([]SandboxRoute, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('sandbox_routing') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []SandboxRoute{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM sandbox_routing ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SandboxRoute{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var item SandboxRoute
		if json.Unmarshal(raw, &item) != nil || !validSandboxBackend(item.Backend) {
			continue
		}
		item.ScopeID = id
		if item.CapabilitiesLost == nil {
			item.CapabilitiesLost = []string{}
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ScopeID < result[j].ScopeID })
	return result, nil
}

func validSandboxBackend(value string) bool {
	return value == "sprites" || value == "aws" || value == "local"
}
