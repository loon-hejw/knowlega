package data

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// SoulRepository operates on Node's soul_configs durable map. Keep the JSON
// document shape and durable_map_versions invalidation compatible with
// resolution/config-store.ts: Node keeps a short-lived map cache and its SOUL
// revision history is embedded in the current document.
type SoulRepository struct{ pg *Postgres }

type Soul struct {
	ScopeID string `json:"scopeId"`
	Content string `json:"content"`
	Version int    `json:"version"`
}

type SoulRevision struct {
	ScopeID   string `json:"scopeId"`
	Content   string `json:"content"`
	Version   int    `json:"version"`
	UpdatedAt int64  `json:"updatedAt"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

type persistedSoul struct {
	Soul
	UpdatedAt  int64          `json:"updatedAt,omitempty"`
	UpdatedBy  string         `json:"updatedBy,omitempty"`
	History    []SoulRevision `json:"history,omitempty"`
	MutationID string         `json:"mutationId,omitempty"`
}

func NewSoulRepository(pg *Postgres) *SoulRepository { return &SoulRepository{pg: pg} }

func (r *SoulRepository) Get(ctx context.Context, scopeID string) (*Soul, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('soul_configs') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM soul_configs WHERE id=$1", scopeID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var item Soul
	if json.Unmarshal(raw, &item) != nil {
		return nil, nil
	}
	if item.ScopeID == "" {
		item.ScopeID = scopeID
	}
	return &item, nil
}

// UpdateLatest mirrors ConfigStore.setSoulLatest. It serializes a scope with
// the same governance advisory-lock key used by the application, retains any
// legacy soul_history revisions only when an older current document has no
// embedded history, and increments the shared durable-map cache version.
func (r *SoulRepository) UpdateLatest(ctx context.Context, scopeID, content, updatedBy string) (int, error) {
	exists, err := r.tableExists(ctx, "soul_configs")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, errors.New("SOUL store is not initialized")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "admin-governance:"+scopeID); err != nil {
		return 0, err
	}
	// createPostgresMap.withBump performs this in every durable map mutation.
	if _, err := tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('soul_configs',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`); err != nil {
		return 0, err
	}

	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM soul_configs WHERE id=$1 FOR UPDATE", scopeID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		revision := newSoulRevision(scopeID, content, 1, updatedBy)
		id, randomErr := mutationID()
		if randomErr != nil {
			return 0, randomErr
		}
		next := persistedSoul{Soul: Soul{ScopeID: scopeID, Content: content, Version: 1}, UpdatedAt: revision.UpdatedAt, UpdatedBy: updatedBy, History: []SoulRevision{revision}, MutationID: id}
		encoded, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			return 0, marshalErr
		}
		if _, err = tx.Exec(ctx, "INSERT INTO soul_configs(id,json) VALUES($1,$2::jsonb)", scopeID, encoded); err != nil {
			return 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return next.Version, nil
	}
	if err != nil {
		return 0, err
	}
	var current persistedSoul
	if err := json.Unmarshal(raw, &current); err != nil {
		return 0, errors.New("SOUL JSON is invalid")
	}
	if current.ScopeID == "" {
		current.ScopeID = scopeID
	}
	legacy := []SoulRevision{}
	if current.History == nil {
		legacy, err = r.legacyHistory(ctx, tx, scopeID)
		if err != nil {
			return 0, err
		}
	}
	history := historyIncludingCurrent(current, legacy)
	revision := newSoulRevision(scopeID, content, current.Version+1, updatedBy)
	id, err := mutationID()
	if err != nil {
		return 0, err
	}
	next := persistedSoul{Soul: Soul{ScopeID: scopeID, Content: content, Version: revision.Version}, UpdatedAt: revision.UpdatedAt, UpdatedBy: updatedBy, History: append([]SoulRevision{revision}, history...), MutationID: id}
	encoded, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, "UPDATE soul_configs SET json=$2::jsonb WHERE id=$1", scopeID, encoded); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return next.Version, nil
}

func (r *SoulRepository) legacyHistory(ctx context.Context, tx pgx.Tx, scopeID string) ([]SoulRevision, error) {
	exists, err := r.tableExists(ctx, "soul_history")
	if err != nil || !exists {
		return []SoulRevision{}, err
	}
	rows, err := tx.Query(ctx, "SELECT json FROM soul_history WHERE json->>'scopeId'=$1", scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SoulRevision{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var revision SoulRevision
		if json.Unmarshal(raw, &revision) == nil && revision.Version > 0 {
			result = append(result, revision)
		}
	}
	return result, rows.Err()
}

func (r *SoulRepository) tableExists(ctx context.Context, table string) (bool, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func newSoulRevision(scopeID, content string, version int, updatedBy string) SoulRevision {
	revision := SoulRevision{ScopeID: scopeID, Content: content, Version: version, UpdatedAt: time.Now().UnixMilli()}
	if updatedBy != "" {
		revision.UpdatedBy = updatedBy
	}
	return revision
}

func historyIncludingCurrent(current persistedSoul, legacy []SoulRevision) []SoulRevision {
	byVersion := map[int]SoulRevision{}
	for _, revision := range legacy {
		byVersion[revision.Version] = revision
	}
	for _, revision := range current.History {
		byVersion[revision.Version] = revision
	}
	if _, exists := byVersion[current.Version]; !exists {
		byVersion[current.Version] = SoulRevision{ScopeID: current.ScopeID, Content: current.Content, Version: current.Version, UpdatedAt: current.UpdatedAt, UpdatedBy: current.UpdatedBy}
	}
	result := make([]SoulRevision, 0, len(byVersion))
	for _, revision := range byVersion {
		result = append(result, revision)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version > result[j].Version })
	return result
}

func mutationID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
