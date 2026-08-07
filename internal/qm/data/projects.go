package data

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/jackc/pgx/v5"
)

type ProjectRepository struct {
	pg    *Postgres
	orgID string
	now   func() time.Time
}

func NewProjectRepository(pg *Postgres, orgID string) *ProjectRepository {
	return &ProjectRepository{pg: pg, orgID: orgID, now: time.Now}
}

func (r *ProjectRepository) Create(ctx context.Context, ownerID, name string) (*biz.Project, error) {
	name = strings.TrimSpace(strings.Join(strings.Fields(name), " "))
	if len(name) > 200 {
		name = name[:200]
	}
	if name == "" {
		return nil, errors.New("project requires name")
	}
	active, err := r.isInternalAndActive(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, nil
	}
	at := r.now().UnixMilli()
	project := biz.Project{ID: uuid.NewString(), OrgID: r.orgID, Name: name, OwnerID: ownerID, MemberIDs: []string{ownerID}, CreatedAt: at, UpdatedAt: at}
	encoded, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "INSERT INTO projects(id,json) VALUES($1,$2::jsonb)", project.ID, encoded); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('projects',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &project, nil
}

func (r *ProjectRepository) ListForMember(ctx context.Context, principalID string) ([]biz.Project, error) {
	if ok, err := r.isActive(ctx, principalID); err != nil || !ok {
		return []biz.Project{}, err
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT json FROM projects WHERE json->>'orgId'=$1`, r.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := make([]biz.Project, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var project biz.Project
		if err := json.Unmarshal(raw, &project); err != nil {
			return nil, err
		}
		if contains(project.MemberIDs, principalID) {
			if active, err := r.isActive(ctx, project.OwnerID); err != nil {
				return nil, err
			} else if active {
				projects = append(projects, project)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].UpdatedAt > projects[j].UpdatedAt })
	return projects, nil
}

func (r *ProjectRepository) AddMember(ctx context.Context, id, actorID, memberID string) (biz.ProjectMutation, error) {
	return r.mutate(ctx, id, actorID, memberID, true)
}

func (r *ProjectRepository) RemoveMember(ctx context.Context, id, actorID, memberID string) (biz.ProjectMutation, error) {
	return r.mutate(ctx, id, actorID, memberID, false)
}

func (r *ProjectRepository) Rename(ctx context.Context, id, actorID, name string) (biz.ProjectMutation, error) {
	tx, err := r.pg.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return biz.ProjectMutation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "project:"+id); err != nil {
		return biz.ProjectMutation{}, err
	}
	project, err := r.lockProject(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ProjectMutation{Status: "not_found"}, nil
	}
	if err != nil {
		return biz.ProjectMutation{}, err
	}
	if !samePerson(project.OwnerID, actorID) || !r.internalAndActiveInTx(ctx, tx, actorID) {
		return biz.ProjectMutation{Status: "forbidden"}, nil
	}
	changed := project.Name != name
	if changed {
		// Node's project store deliberately treats a rename as metadata, not a
		// roster change. Keep updatedAt stable so project scopeVersion capability
		// snapshots remain valid across a name-only change.
		project.Name = name
	}
	if err := r.storeProject(ctx, tx, project); err != nil {
		return biz.ProjectMutation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return biz.ProjectMutation{}, err
	}
	return biz.ProjectMutation{Status: "ok", Project: &project, Changed: changed}, nil
}

func (r *ProjectRepository) HasScopeMembership(ctx context.Context, scopeID, principalID string) (bool, error) {
	const prefix = "group:web-project-"
	if !strings.HasPrefix(scopeID, prefix) {
		return false, nil
	}
	var raw []byte
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM projects WHERE id=$1", strings.TrimPrefix(scopeID, prefix)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var project biz.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return false, err
	}
	if project.OrgID != r.orgID || !contains(project.MemberIDs, principalID) {
		return false, nil
	}
	return r.isActive(ctx, principalID)
}

// AuthorizesScopeVersion checks the project-membership snapshot carried by a
// capability or approval request. Non-project scopes have no roster version
// and are therefore always current, matching the Node scope authorizer.
func (r *ProjectRepository) AuthorizesScopeVersion(ctx context.Context, scopeID, principalID, version string) (bool, error) {
	const prefix = "group:web-project-"
	if !strings.HasPrefix(scopeID, prefix) {
		return true, nil
	}
	var raw []byte
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM projects WHERE id=$1", strings.TrimPrefix(scopeID, prefix)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var project biz.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return false, err
	}
	if project.OrgID != r.orgID || !contains(project.MemberIDs, principalID) || version != strconv.FormatInt(project.UpdatedAt, 10) {
		return false, nil
	}
	ownerActive, err := r.isActive(ctx, project.OwnerID)
	if err != nil || !ownerActive {
		return false, err
	}
	return r.isActive(ctx, principalID)
}

func (r *ProjectRepository) mutate(ctx context.Context, id, actorID, memberID string, add bool) (biz.ProjectMutation, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return biz.ProjectMutation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "project:"+id); err != nil {
		return biz.ProjectMutation{}, err
	}
	project, err := r.lockProject(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ProjectMutation{Status: "not_found"}, nil
	}
	if err != nil {
		return biz.ProjectMutation{}, err
	}
	if !r.internalAndActiveInTx(ctx, tx, actorID) {
		return biz.ProjectMutation{Status: "forbidden"}, nil
	}
	changed := false
	if add {
		if !contains(project.MemberIDs, actorID) {
			return biz.ProjectMutation{Status: "forbidden"}, nil
		}
		if samePerson(memberID, project.OwnerID) {
			return biz.ProjectMutation{Status: "invalid_member"}, nil
		}
		if !r.internalAndActiveInTx(ctx, tx, memberID) {
			return biz.ProjectMutation{Status: "invalid_member"}, nil
		}
		if !contains(project.MemberIDs, memberID) {
			project.MemberIDs = append(project.MemberIDs, memberID)
			project.UpdatedAt = max(r.now().UnixMilli(), project.UpdatedAt+1)
			changed = true
		}
	} else {
		if !samePerson(project.OwnerID, actorID) {
			return biz.ProjectMutation{Status: "forbidden"}, nil
		}
		if samePerson(memberID, project.OwnerID) {
			return biz.ProjectMutation{Status: "invalid_member"}, nil
		}
		if contains(project.MemberIDs, memberID) {
			project.MemberIDs = without(project.MemberIDs, memberID)
			project.UpdatedAt = max(r.now().UnixMilli(), project.UpdatedAt+1)
			changed = true
		}
	}
	if err := r.storeProject(ctx, tx, project); err != nil {
		return biz.ProjectMutation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return biz.ProjectMutation{}, err
	}
	return biz.ProjectMutation{Status: "ok", Project: &project, Changed: changed}, nil
}

func (r *ProjectRepository) lockProject(ctx context.Context, tx pgx.Tx, id string) (biz.Project, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT json FROM projects WHERE id=$1 FOR UPDATE", id).Scan(&raw); err != nil {
		return biz.Project{}, err
	}
	var project biz.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return biz.Project{}, err
	}
	if project.OrgID != r.orgID {
		return biz.Project{}, pgx.ErrNoRows
	}
	return project, nil
}

func (r *ProjectRepository) storeProject(ctx context.Context, tx pgx.Tx, project biz.Project) error {
	raw, err := json.Marshal(project)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE projects SET json=$2::jsonb WHERE id=$1", project.ID, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('projects',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`)
	return err
}

func (r *ProjectRepository) isActive(ctx context.Context, principalID string) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM deactivated_principals WHERE id=$1)", strings.ToLower(principalID)).Scan(&exists)
	return !exists, err
}

func (r *ProjectRepository) isInternalAndActive(ctx context.Context, principalID string) (bool, error) {
	var internal bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM directory_members WHERE org_id=$1 AND principal_id=$2 AND type='internal')", r.orgID, principalID).Scan(&internal)
	if err != nil || !internal {
		return false, err
	}
	return r.isActive(ctx, principalID)
}

func (r *ProjectRepository) activeInTx(ctx context.Context, tx pgx.Tx, principalID string) bool {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM deactivated_principals WHERE id=$1)", strings.ToLower(principalID)).Scan(&exists); err != nil {
		return false
	}
	return !exists
}

func (r *ProjectRepository) internalAndActiveInTx(ctx context.Context, tx pgx.Tx, principalID string) bool {
	var internal bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM directory_members WHERE org_id=$1 AND principal_id=$2 AND type='internal')", r.orgID, principalID).Scan(&internal); err != nil || !internal {
		return false
	}
	return r.activeInTx(ctx, tx, principalID)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if samePerson(candidate, value) {
			return true
		}
	}
	return false
}
func without(values []string, value string) []string {
	result := make([]string, 0, len(values))
	for _, candidate := range values {
		if !samePerson(candidate, value) {
			result = append(result, candidate)
		}
	}
	return result
}

func samePerson(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if strings.Contains(left, "@") || strings.Contains(right, "@") {
		return strings.EqualFold(left, right)
	}
	return left == right
}
