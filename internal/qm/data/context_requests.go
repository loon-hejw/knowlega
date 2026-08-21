package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ContextRequestExpiry = 2 * time.Minute

type ContextRequest struct {
	ID        string          `json:"id"`
	Source    string          `json:"source"`
	CreatedAt int64           `json:"createdAt"`
	Status    string          `json:"status"`
	Query     json.RawMessage `json:"query"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *string         `json:"error,omitempty"`
}

type PendingContextRequest struct {
	ContextRequest
	ViewerTokenEncrypted string
}

type ContextRequestRepository struct{ pg *Postgres }

func NewContextRequestRepository(pg *Postgres) *ContextRequestRepository {
	return &ContextRequestRepository{pg: pg}
}

func (r *ContextRequestRepository) Create(ctx context.Context, source string, query json.RawMessage, viewerTokenEncrypted string) (*ContextRequest, error) {
	source = strings.TrimSpace(source)
	if source == "" || !json.Valid(query) {
		return nil, errors.New("context request source and JSON query are required")
	}
	request := ContextRequest{
		ID:        uuid.NewString(),
		Source:    source,
		CreatedAt: time.Now().UnixMilli(),
		Status:    "pending",
		Query:     query,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "INSERT INTO context_requests(id,json) VALUES($1,$2::jsonb)", request.ID, payload); err != nil {
		return nil, err
	}
	if viewerTokenEncrypted != "" {
		if _, err := tx.Exec(ctx, "INSERT INTO context_request_tokens(request_id,token_enc,created_at) VALUES($1,$2,$3)", request.ID, viewerTokenEncrypted, request.CreatedAt); err != nil {
			return nil, err
		}
	}
	if err := bumpDurableMapVersion(ctx, tx, "context_requests"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &request, nil
}

func (r *ContextRequestRepository) Get(ctx context.Context, id string) (*ContextRequest, error) {
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM context_requests WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeContextRequest(raw)
}

func (r *ContextRequestRepository) Delete(ctx context.Context, id string) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bumpDurableMapVersion(ctx, tx, "context_requests"); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "DELETE FROM context_requests WHERE id=$1", id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

func (r *ContextRequestRepository) Pending(ctx context.Context, source string, now time.Time) ([]PendingContextRequest, error) {
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-ContextRequestExpiry).UnixMilli()
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deleted, err := tx.Exec(ctx, `DELETE FROM context_requests
WHERE json->>'status'='pending' AND (json->>'createdAt') ~ '^[0-9]+$' AND (json->>'createdAt')::bigint < $1`, cutoff)
	if err != nil {
		return nil, err
	}
	if deleted.RowsAffected() > 0 {
		if err := bumpDurableMapVersion(ctx, tx, "context_requests"); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT c.json,COALESCE(t.token_enc,'') FROM context_requests c
LEFT JOIN context_request_tokens t ON t.request_id=c.id
WHERE c.json->>'source'=$1 AND c.json->>'status'='pending' AND (c.json->>'createdAt') ~ '^[0-9]+$' AND (c.json->>'createdAt')::bigint >= $2
ORDER BY c.id`, source, cutoff)
	if err != nil {
		return nil, err
	}
	var pending []PendingContextRequest
	for rows.Next() {
		var raw json.RawMessage
		var item PendingContextRequest
		if err := rows.Scan(&raw, &item.ViewerTokenEncrypted); err != nil {
			return nil, err
		}
		request, err := decodeContextRequest(raw)
		if err != nil {
			return nil, err
		}
		item.ContextRequest = *request
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pending, nil
}

func (r *ContextRequestRepository) Fulfill(ctx context.Context, id string, result json.RawMessage, failure *string) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bumpDurableMapVersion(ctx, tx, "context_requests"); err != nil {
		return false, err
	}
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM context_requests WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	request, err := decodeContextRequest(raw)
	if err != nil {
		return false, err
	}
	request.Error = nil
	request.Result = nil
	if failure != nil {
		request.Status = "failed"
		request.Error = failure
	} else {
		if !json.Valid(result) {
			return false, errors.New("context request result must be valid JSON")
		}
		request.Status = "done"
		request.Result = result
	}
	updated, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE context_requests SET json=$2::jsonb WHERE id=$1", id, updated); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM context_request_tokens WHERE request_id=$1", id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func decodeContextRequest(raw json.RawMessage) (*ContextRequest, error) {
	var request ContextRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if request.ID == "" || request.Source == "" || request.Status == "" || !json.Valid(request.Query) {
		return nil, errors.New("malformed context request")
	}
	return &request, nil
}
