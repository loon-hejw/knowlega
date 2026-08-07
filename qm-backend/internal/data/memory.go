package data

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// MemoryHead is the current revision of one Node-compatible memory scope.
// Revision is the durable sequence number, encoded as Node's string token.
type MemoryHead struct {
	Content   string
	Revision  string
	UpdatedAt *int64
}

type MemoryRevision struct {
	Revision  string
	Content   string
	Operation string
	Author    *string
	At        int64
}

type MemoryScopeMetadata struct {
	ScopeID   string
	Bytes     int64
	UpdatedAt *int64
}

type MemoryRepository struct{ pg *Postgres }

func NewMemoryRepository(pg *Postgres) *MemoryRepository { return &MemoryRepository{pg: pg} }

func (r *MemoryRepository) Head(ctx context.Context, scopeID string) (MemoryHead, error) {
	var content string
	var seq, at int64
	err := r.pg.Pool.QueryRow(ctx, `SELECT body,seq,at FROM memory_revisions
WHERE scope_id=$1 ORDER BY seq DESC LIMIT 1`, scopeID).Scan(&content, &seq, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryHead{Content: "", Revision: "0"}, nil
	}
	if err != nil {
		return MemoryHead{}, err
	}
	return MemoryHead{Content: content, Revision: strconv.FormatInt(seq, 10), UpdatedAt: &at}, nil
}

// Replace stores a new head only when its normalized content differs from the
// current head. This preserves Node's no-op replace behavior.
func (r *MemoryRepository) Replace(ctx context.Context, scopeID, content, author string) error {
	_, err := r.conditionalReplace(ctx, scopeID, content, -1, author, "replace", false)
	return err
}

// ReplaceIfRevision applies a Node-compatible optimistic replacement. An
// invalid revision, stale head, or missing expected revision returns false.
func (r *MemoryRepository) ReplaceIfRevision(ctx context.Context, scopeID, content, revision, author string) (bool, error) {
	expected, err := parseRevision(revision)
	if err != nil {
		return false, nil
	}
	return r.conditionalReplace(ctx, scopeID, content, expected, author, "replace", true)
}

func (r *MemoryRepository) History(ctx context.Context, scopeID string, limit int) ([]MemoryRevision, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT seq,body,op,author,at FROM memory_revisions
WHERE scope_id=$1 ORDER BY seq DESC LIMIT $2`, scopeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MemoryRevision{}
	for rows.Next() {
		var item MemoryRevision
		var seq int64
		if err := rows.Scan(&seq, &item.Content, &item.Operation, &item.Author, &item.At); err != nil {
			return nil, err
		}
		item.Revision = strconv.FormatInt(seq, 10)
		result = append(result, item)
	}
	return result, rows.Err()
}

// Metadata returns the current body size and update time for every scope with
// a persisted revision. It mirrors the Node PostgreSQL memory store's
// DISTINCT ON metadata query without loading private content.
func (r *MemoryRepository) Metadata(ctx context.Context) ([]MemoryScopeMetadata, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT DISTINCT ON (scope_id) scope_id,octet_length(body),at
FROM memory_revisions ORDER BY scope_id,seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MemoryScopeMetadata{}
	for rows.Next() {
		var item MemoryScopeMetadata
		if err := rows.Scan(&item.ScopeID, &item.Bytes, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// Restore first resolves the requested historic body, then conditionally writes
// it as a new revision. It intentionally does not mutate the historic row.
func (r *MemoryRepository) Restore(ctx context.Context, scopeID, revision, expectedRevision, author string) (bool, error) {
	revisionSeq, err := parseRevision(revision)
	if err != nil {
		return false, nil
	}
	expectedSeq, err := parseRevision(expectedRevision)
	if err != nil {
		return false, nil
	}
	var content string
	err = r.pg.Pool.QueryRow(ctx, "SELECT body FROM memory_revisions WHERE scope_id=$1 AND seq=$2", scopeID, revisionSeq).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.conditionalReplace(ctx, scopeID, content, expectedSeq, author, "restore", true)
}

func (r *MemoryRepository) conditionalReplace(ctx context.Context, scopeID, content string, expected int64, author, operation string, conditional bool) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('memory'), hashtext($1))", scopeID); err != nil {
		return false, err
	}
	var current string
	var seq int64
	err = tx.QueryRow(ctx, `SELECT body,seq FROM memory_revisions WHERE scope_id=$1
ORDER BY seq DESC LIMIT 1`, scopeID).Scan(&current, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		current, seq, err = "", 0, nil
	}
	if err != nil {
		return false, err
	}
	if conditional && seq != expected {
		return false, tx.Rollback(ctx)
	}
	next := normalizeMemoryReplace(content)
	if next != current {
		if _, err := tx.Exec(ctx, `INSERT INTO memory_revisions(scope_id,seq,op,body,author,at)
VALUES($1,$2,$3,$4,NULLIF($5,''),$6)`, scopeID, seq+1, operation, next, author, time.Now().UnixMilli()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func parseRevision(value string) (int64, error) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errors.New("invalid revision")
	}
	return strconv.ParseInt(value, 10, 64)
}

func normalizeMemoryReplace(content string) string {
	trimmed := strings.TrimRightFunc(content, unicode.IsSpace)
	if trimmed == "" {
		return ""
	}
	return trimmed + "\n"
}
