package data

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Environment struct {
	ID           string  `json:"id"`
	OrgID        string  `json:"orgId"`
	Name         *string `json:"name"`
	OwnerActorID *string `json:"ownerActorId"`
	CreatedAt    int64   `json:"createdAt"`
	UpdatedAt    int64   `json:"updatedAt"`
}

type EnvironmentAttachment struct {
	ScopeID       string `json:"scopeId"`
	EnvironmentID string `json:"environmentId"`
	AttachedBy    string `json:"attachedBy"`
	AttachedAt    int64  `json:"attachedAt"`
}

type EnvironmentRepository struct {
	pg    *Postgres
	orgID string
	now   func() time.Time
}

func NewEnvironmentRepository(pg *Postgres, orgID string) *EnvironmentRepository {
	return &EnvironmentRepository{pg: pg, orgID: orgID, now: time.Now}
}

func (r *EnvironmentRepository) Create(ctx context.Context, id, name, ownerActorID string) (*Environment, error) {
	id, name, ownerActorID = strings.TrimSpace(id), strings.TrimSpace(name), strings.TrimSpace(ownerActorID)
	if id == "" || name == "" || ownerActorID == "" {
		return nil, errors.New("environment id, name, and owner are required")
	}
	at := r.now().UnixMilli()
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO environments(id,org_id,name,owner_actor_id,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(id) DO NOTHING`, id, r.orgID, name, ownerActorID, at)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *EnvironmentRepository) Get(ctx context.Context, id string) (*Environment, error) {
	row := r.pg.Pool.QueryRow(ctx, "SELECT id,org_id,name,owner_actor_id,created_at,updated_at FROM environments WHERE id=$1 AND org_id=$2", strings.TrimSpace(id), r.orgID)
	return scanEnvironment(row)
}

func (r *EnvironmentRepository) List(ctx context.Context) ([]Environment, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,org_id,name,owner_actor_id,created_at,updated_at FROM environments WHERE org_id=$1 ORDER BY updated_at DESC", r.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Environment, 0)
	for rows.Next() {
		environment, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *environment)
	}
	return result, rows.Err()
}

func (r *EnvironmentRepository) FindByName(ctx context.Context, name string) (*Environment, error) {
	row := r.pg.Pool.QueryRow(ctx, "SELECT id,org_id,name,owner_actor_id,created_at,updated_at FROM environments WHERE org_id=$1 AND name=$2 ORDER BY updated_at DESC LIMIT 1", r.orgID, strings.TrimSpace(name))
	return scanEnvironment(row)
}

func (r *EnvironmentRepository) Attach(ctx context.Context, scopeID, environmentID, actorID string) error {
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO environment_attachments(scope_id,environment_id,attached_by,attached_at)
VALUES($1,$2,$3,$4) ON CONFLICT(scope_id) DO UPDATE SET environment_id=EXCLUDED.environment_id,attached_by=EXCLUDED.attached_by,attached_at=EXCLUDED.attached_at`, strings.TrimSpace(scopeID), strings.TrimSpace(environmentID), strings.TrimSpace(actorID), r.now().UnixMilli())
	return err
}

func (r *EnvironmentRepository) AttachmentsFor(ctx context.Context, environmentID string) ([]EnvironmentAttachment, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT scope_id,environment_id,attached_by,attached_at FROM environment_attachments WHERE environment_id=$1", environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]EnvironmentAttachment, 0)
	for rows.Next() {
		var attachment EnvironmentAttachment
		if err := rows.Scan(&attachment.ScopeID, &attachment.EnvironmentID, &attachment.AttachedBy, &attachment.AttachedAt); err != nil {
			return nil, err
		}
		result = append(result, attachment)
	}
	return result, rows.Err()
}

type environmentScanner interface{ Scan(...any) error }

func scanEnvironment(row environmentScanner) (*Environment, error) {
	var environment Environment
	var name, owner sql.NullString
	if err := row.Scan(&environment.ID, &environment.OrgID, &name, &owner, &environment.CreatedAt, &environment.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if name.Valid {
		environment.Name = &name.String
	}
	if owner.Valid {
		environment.OwnerActorID = &owner.String
	}
	return &environment, nil
}
