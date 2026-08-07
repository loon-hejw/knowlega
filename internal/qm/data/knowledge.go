package data

import (
	"context"
	"time"
)

type KnowledgeScope struct {
	OrgID           string
	ExternalScopeID string
	Kind            string
	ProjectID       string
	ProjectName     string
	RootPath        string
	Status          string
}

type KnowledgeScopeRepository struct{ pg *Postgres }

func NewKnowledgeScopeRepository(pg *Postgres) *KnowledgeScopeRepository {
	return &KnowledgeScopeRepository{pg: pg}
}

func (r *KnowledgeScopeRepository) Ensure(ctx context.Context, scope KnowledgeScope) (KnowledgeScope, error) {
	now := time.Now().UnixMilli()
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO knowledge_scopes(org_id,external_scope_id,scope_kind,project_id,project_name,root_path,status,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,'active',$7,$7)
ON CONFLICT(org_id,external_scope_id,scope_kind) DO UPDATE SET project_name=EXCLUDED.project_name,updated_at=EXCLUDED.updated_at`, scope.OrgID, scope.ExternalScopeID, scope.Kind, scope.ProjectID, scope.ProjectName, scope.RootPath, now)
	if err != nil {
		return KnowledgeScope{}, err
	}
	return r.Get(ctx, scope.OrgID, scope.ExternalScopeID, scope.Kind)
}

func (r *KnowledgeScopeRepository) Get(ctx context.Context, orgID, externalScopeID, kind string) (KnowledgeScope, error) {
	var scope KnowledgeScope
	err := r.pg.Pool.QueryRow(ctx, `SELECT org_id,external_scope_id,scope_kind,project_id,project_name,root_path,status
FROM knowledge_scopes WHERE org_id=$1 AND external_scope_id=$2 AND scope_kind=$3`, orgID, externalScopeID, kind).Scan(&scope.OrgID, &scope.ExternalScopeID, &scope.Kind, &scope.ProjectID, &scope.ProjectName, &scope.RootPath, &scope.Status)
	return scope, err
}

func (r *KnowledgeScopeRepository) List(ctx context.Context, orgID string) ([]KnowledgeScope, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT org_id,external_scope_id,scope_kind,project_id,project_name,root_path,status
FROM knowledge_scopes WHERE org_id=$1 ORDER BY external_scope_id,scope_kind`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []KnowledgeScope
	for rows.Next() {
		var scope KnowledgeScope
		if err := rows.Scan(&scope.OrgID, &scope.ExternalScopeID, &scope.Kind, &scope.ProjectID, &scope.ProjectName, &scope.RootPath, &scope.Status); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}
