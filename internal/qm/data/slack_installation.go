package data

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SlackInstallationRepository reads the metadata-only Node durable record.
// Bot and app token ciphertext is deliberately neither decrypted nor returned.
type SlackInstallationRepository struct{ pg *Postgres }

type SlackInstallationStatus struct {
	Configured bool
	Managed    bool
	TeamID     string
	TeamName   string
	UpdatedAt  *int64
	UpdatedBy  string
	Version    string
}

func NewSlackInstallationRepository(pg *Postgres) *SlackInstallationRepository {
	return &SlackInstallationRepository{pg: pg}
}

// Status is nil when the admin durable record does not exist. The caller then
// selects the explicit environment fallback declared for the Go cutover.
func (r *SlackInstallationRepository) Status(ctx context.Context, orgID string) (*SlackInstallationStatus, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('slack_installation') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM slack_installation WHERE id=$1", orgID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record struct {
		Disabled  bool   `json:"disabled"`
		TeamID    string `json:"teamId"`
		TeamName  string `json:"teamName"`
		UpdatedAt *int64 `json:"updatedAt"`
		UpdatedBy string `json:"updatedBy"`
		Version   string `json:"version"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return nil, nil
	}
	if record.Disabled {
		return &SlackInstallationStatus{Configured: false, Managed: true}, nil
	}
	return &SlackInstallationStatus{Configured: true, Managed: true, TeamID: record.TeamID, TeamName: record.TeamName, UpdatedAt: record.UpdatedAt, UpdatedBy: record.UpdatedBy, Version: record.Version}, nil
}
