package data

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
)

var (
	deploymentNamePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)
	deploymentUUIDPattern    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	deploymentControlPattern = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// Deployment is the read-only control-plane projection stored in Node's
// durable `deployments` map. Runtime endpoints and Git workspaces are not
// represented here.
type Deployment struct {
	ID, OwnerScopeID, CreatedBy, Name, DisplayName, Status string
	CreatedInScope                                         *string
	Endpoint                                               *DeploymentEndpoint
	CurrentVersion                                         int
	AppliedVersion                                         *int
	LastAccessAt                                           *int64
	Versions                                               []DeploymentVersion
}

type DeploymentEndpoint struct {
	PublicURL *string
}

type DeploymentVersion struct {
	Version              int
	CreatedAt            int64
	Commit, ParentCommit *string
}

type DeploymentRepository struct{ pg *Postgres }

func NewDeploymentRepository(pg *Postgres) *DeploymentRepository {
	return &DeploymentRepository{pg: pg}
}

func (r *DeploymentRepository) List(ctx context.Context) ([]Deployment, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('deployments') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []Deployment{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT json FROM deployments")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Deployment{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item Deployment
		if json.Unmarshal(raw, &item) != nil || item.ID == "" || item.OwnerScopeID == "" || item.CreatedBy == "" {
			continue
		}
		if item.Versions == nil {
			item.Versions = []DeploymentVersion{}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *DeploymentRepository) Get(ctx context.Context, idOrName string) (*Deployment, error) {
	items, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID == idOrName || item.Name != "" && item.Name == idOrName {
			return &item, nil
		}
	}
	return nil, nil
}

// UpdateName updates one Node durable-map deployment document while preserving
// every runtime field Go does not own. It takes the same per-deployment
// advisory lock and bumps the shared map version so Node refreshes its cache.
func (r *DeploymentRepository) UpdateName(ctx context.Context, id, name string) (json.RawMessage, *Deployment, error) {
	if err := validateDeploymentName(name); err != nil {
		return nil, nil, err
	}
	return r.updateDocument(ctx, id, func(document map[string]json.RawMessage) error {
		encoded, err := json.Marshal(name)
		if err != nil {
			return err
		}
		document["name"] = encoded
		return nil
	}, name, false)
}

// UpdateDisplayName mirrors Node's setDisplayName: an empty value clears the
// optional JSON property rather than persisting an empty string.
func (r *DeploymentRepository) UpdateDisplayName(ctx context.Context, id, displayName string) (json.RawMessage, *Deployment, error) {
	if err := validateDeploymentDisplayName(displayName); err != nil {
		return nil, nil, err
	}
	return r.updateDocument(ctx, id, func(document map[string]json.RawMessage) error {
		if displayName == "" {
			delete(document, "displayName")
			return nil
		}
		encoded, err := json.Marshal(displayName)
		if err != nil {
			return err
		}
		document["displayName"] = encoded
		return nil
	}, "", true)
}

func (r *DeploymentRepository) updateDocument(ctx context.Context, id string, mutate func(map[string]json.RawMessage) error, name string, displayOnly bool) (json.RawMessage, *Deployment, error) {
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return nil, nil, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "deploy:"+id); err != nil {
		return nil, nil, err
	}
	if !displayOnly {
		var conflict bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM deployments WHERE id<>$1 AND json->>'name'=$2)", id, name).Scan(&conflict); err != nil {
			return nil, nil, err
		}
		if conflict {
			return nil, nil, errors.New("deployment name taken: " + name)
		}
	}
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM deployments WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil || document == nil {
		return nil, nil, errors.New("deployment JSON must be an object")
	}
	if err := mutate(document); err != nil {
		return nil, nil, err
	}
	updated, err := json.Marshal(document)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('deployments',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, "UPDATE deployments SET json=$2::jsonb WHERE id=$1", id, updated); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	var deployment Deployment
	if json.Unmarshal(updated, &deployment) != nil || deployment.ID == "" {
		return nil, nil, errors.New("deployment JSON is invalid")
	}
	return updated, &deployment, nil
}

func (r *DeploymentRepository) tableExists(ctx context.Context) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('deployments') IS NOT NULL").Scan(&exists)
	return exists, err
}

func validateDeploymentName(name string) error {
	if deploymentUUIDPattern.MatchString(name) {
		return errors.New("invalid deployment name (looks like an id)")
	}
	if !deploymentNamePattern.MatchString(name) {
		return errors.New(`invalid deployment name "` + name + `": use 2-40 chars, lowercase letters/digits/hyphens, no leading/trailing hyphen`)
	}
	return nil
}

func validateDeploymentDisplayName(value string) error {
	if deploymentControlPattern.MatchString(value) {
		return errors.New("invalid display name: no control characters")
	}
	if len(utf16.Encode([]rune(value))) > 60 {
		return errors.New("display name too long (max 60 chars)")
	}
	return nil
}
