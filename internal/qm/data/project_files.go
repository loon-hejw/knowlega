package data

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ProjectFileMembership struct {
	ProjectID          string  `json:"projectId"`
	ProjectScopeID     string  `json:"projectScopeId"`
	FileID             string  `json:"fileId"`
	KnowledgeProjectID string  `json:"knowledgeProjectId"`
	SourceSHA256       string  `json:"sourceSha256"`
	RawPath            *string `json:"rawPath,omitempty"`
	QueueTaskID        *string `json:"queueTaskId,omitempty"`
	Status             string  `json:"status"`
	GeneratedPageCount int     `json:"generatedPageCount"`
	LastError          *string `json:"lastError,omitempty"`
	CreatedAt          int64   `json:"createdAt"`
	UpdatedAt          int64   `json:"updatedAt"`
}

type ProjectFileMembershipRepository struct{ pg *Postgres }

func NewProjectFileMembershipRepository(pg *Postgres) *ProjectFileMembershipRepository {
	return &ProjectFileMembershipRepository{pg: pg}
}

func (r *ProjectFileMembershipRepository) PutQueued(ctx context.Context, membership ProjectFileMembership) (ProjectFileMembership, error) {
	now := time.Now().UnixMilli()
	if membership.CreatedAt == 0 {
		membership.CreatedAt = now
	}
	membership.UpdatedAt = now
	membership.Status = "queued"
	membership.GeneratedPageCount = 0
	membership.LastError = nil
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO project_file_memberships(project_id,project_scope_id,file_id,knowledge_project_id,source_sha256,raw_path,queue_task_id,status,generated_page_count,last_error,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT(project_id,file_id) DO UPDATE SET project_scope_id=EXCLUDED.project_scope_id,knowledge_project_id=EXCLUDED.knowledge_project_id,source_sha256=EXCLUDED.source_sha256,raw_path=NULL,queue_task_id=NULL,status='queued',generated_page_count=0,last_error=NULL,updated_at=EXCLUDED.updated_at`,
		membership.ProjectID, membership.ProjectScopeID, membership.FileID, membership.KnowledgeProjectID, membership.SourceSHA256, membership.RawPath, membership.QueueTaskID, membership.Status, membership.GeneratedPageCount, membership.LastError, membership.CreatedAt, membership.UpdatedAt)
	if err != nil {
		return ProjectFileMembership{}, err
	}
	return r.Get(ctx, membership.ProjectID, membership.FileID)
}

func (r *ProjectFileMembershipRepository) Get(ctx context.Context, projectID, fileID string) (ProjectFileMembership, error) {
	var item ProjectFileMembership
	err := scanProjectFileMembership(r.pg.Pool.QueryRow(ctx, `SELECT project_id,project_scope_id,file_id,knowledge_project_id,source_sha256,raw_path,queue_task_id,status,generated_page_count,last_error,created_at,updated_at
FROM project_file_memberships WHERE project_id=$1 AND file_id=$2`, projectID, fileID), &item)
	return item, err
}

func (r *ProjectFileMembershipRepository) Find(ctx context.Context, projectID, fileID string) (*ProjectFileMembership, error) {
	item, err := r.Get(ctx, projectID, fileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *ProjectFileMembershipRepository) ListByProject(ctx context.Context, projectID string) ([]ProjectFileMembership, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT project_id,project_scope_id,file_id,knowledge_project_id,source_sha256,raw_path,queue_task_id,status,generated_page_count,last_error,created_at,updated_at
FROM project_file_memberships WHERE project_id=$1 ORDER BY created_at DESC,file_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjectFileMemberships(rows)
}

func (r *ProjectFileMembershipRepository) ListByFile(ctx context.Context, fileID string) ([]ProjectFileMembership, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT project_id,project_scope_id,file_id,knowledge_project_id,source_sha256,raw_path,queue_task_id,status,generated_page_count,last_error,created_at,updated_at
FROM project_file_memberships WHERE file_id=$1 ORDER BY project_id`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjectFileMemberships(rows)
}

func (r *ProjectFileMembershipRepository) SetState(ctx context.Context, projectID, fileID, status string, rawPath, queueTaskID *string, generatedPageCount int, lastError *string) error {
	command, err := r.pg.Pool.Exec(ctx, `UPDATE project_file_memberships SET status=$3,raw_path=COALESCE($4,raw_path),queue_task_id=COALESCE($5,queue_task_id),generated_page_count=$6,last_error=$7,updated_at=$8
WHERE project_id=$1 AND file_id=$2`, projectID, fileID, status, rawPath, queueTaskID, generatedPageCount, lastError, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *ProjectFileMembershipRepository) Delete(ctx context.Context, projectID, fileID string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM project_file_memberships WHERE project_id=$1 AND file_id=$2", projectID, fileID)
	return err
}

func (r *ProjectFileMembershipRepository) MarkRunnableProcessing(ctx context.Context, projectID string) error {
	_, err := r.ClaimRunnableProcessing(ctx, projectID)
	return err
}

// ClaimRunnableProcessing atomically moves the memberships owned by this
// worker tick into processing and returns exactly that claimed set. Historical
// ready memberships are deliberately excluded so a later maintenance failure
// cannot make unrelated project files look unhealthy.
func (r *ProjectFileMembershipRepository) ClaimRunnableProcessing(ctx context.Context, projectID string) ([]ProjectFileMembership, error) {
	rows, err := r.pg.Pool.Query(ctx, `UPDATE project_file_memberships
SET status='processing',last_error=NULL,updated_at=$2
WHERE project_id=$1 AND status IN ('queued','failed') AND queue_task_id IS NOT NULL
RETURNING project_id,project_scope_id,file_id,knowledge_project_id,source_sha256,raw_path,queue_task_id,status,generated_page_count,last_error,created_at,updated_at`, projectID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjectFileMemberships(rows)
}

// MarkClaimedFailed records a maintenance failure only on memberships claimed
// by the current worker tick. Queue task identity prevents an unrelated newer
// attachment from being overwritten if project membership changes mid-tick.
func (r *ProjectFileMembershipRepository) MarkClaimedFailed(ctx context.Context, claimed []ProjectFileMembership, message string) error {
	if len(claimed) == 0 {
		return nil
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UnixMilli()
	for _, item := range claimed {
		if item.QueueTaskID == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE project_file_memberships
SET status='failed',last_error=$4,updated_at=$5
WHERE project_id=$1 AND file_id=$2 AND queue_task_id=$3`, item.ProjectID, item.FileID, *item.QueueTaskID, message, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *ProjectFileMembershipRepository) RequeueProcessing(ctx context.Context, projectID string) error {
	_, err := r.pg.Pool.Exec(ctx, "UPDATE project_file_memberships SET status='queued',updated_at=$2 WHERE project_id=$1 AND status='processing' AND queue_task_id IS NOT NULL", projectID, time.Now().UnixMilli())
	return err
}

type projectFileMembershipScanner interface{ Scan(...any) error }

func scanProjectFileMembership(row projectFileMembershipScanner, item *ProjectFileMembership) error {
	return row.Scan(&item.ProjectID, &item.ProjectScopeID, &item.FileID, &item.KnowledgeProjectID, &item.SourceSHA256, &item.RawPath, &item.QueueTaskID, &item.Status, &item.GeneratedPageCount, &item.LastError, &item.CreatedAt, &item.UpdatedAt)
}

func collectProjectFileMemberships(rows pgx.Rows) ([]ProjectFileMembership, error) {
	items := []ProjectFileMembership{}
	for rows.Next() {
		var item ProjectFileMembership
		if err := scanProjectFileMembership(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
