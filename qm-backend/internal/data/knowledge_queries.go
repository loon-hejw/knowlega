package data

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type KnowledgeQueryRun struct {
	ID              string
	OrgID           string
	ExternalScopeID string
	Kind            string
	Question        string
	Answer          string
	CitationPaths   []string
	QueryPlan       string
	CanWriteBack    bool
}

type KnowledgeQueryRepository struct{ pg *Postgres }

func NewKnowledgeQueryRepository(pg *Postgres) *KnowledgeQueryRepository {
	return &KnowledgeQueryRepository{pg: pg}
}

func (r *KnowledgeQueryRepository) Create(ctx context.Context, query KnowledgeQueryRun) (KnowledgeQueryRun, error) {
	query.ID = uuid.NewString()
	citations, err := json.Marshal(query.CitationPaths)
	if err != nil {
		return KnowledgeQueryRun{}, err
	}
	_, err = r.pg.Pool.Exec(ctx, `INSERT INTO knowledge_query_runs(id,org_id,external_scope_id,scope_kind,question,answer,citation_paths,query_plan,can_write_back,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10)`, query.ID, query.OrgID, query.ExternalScopeID, query.Kind, query.Question, query.Answer, string(citations), query.QueryPlan, query.CanWriteBack, time.Now().UnixMilli())
	return query, err
}

func (r *KnowledgeQueryRepository) Get(ctx context.Context, id, orgID, externalScopeID, kind string) (KnowledgeQueryRun, error) {
	var query KnowledgeQueryRun
	var citations []byte
	err := r.pg.Pool.QueryRow(ctx, `SELECT id,org_id,external_scope_id,scope_kind,question,answer,citation_paths,query_plan,can_write_back
FROM knowledge_query_runs WHERE id=$1 AND org_id=$2 AND external_scope_id=$3 AND scope_kind=$4`, id, orgID, externalScopeID, kind).Scan(&query.ID, &query.OrgID, &query.ExternalScopeID, &query.Kind, &query.Question, &query.Answer, &citations, &query.QueryPlan, &query.CanWriteBack)
	if err != nil {
		return KnowledgeQueryRun{}, err
	}
	if err := json.Unmarshal(citations, &query.CitationPaths); err != nil {
		return KnowledgeQueryRun{}, err
	}
	return query, nil
}
