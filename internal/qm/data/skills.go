package data

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SkillRepository reads Node's durable skill and skill-pack maps. Execution,
// signatures, publication transitions, and pack synchronization remain in the
// Node runtime during the migration.
type SkillRepository struct{ pg *Postgres }

type SkillManifest struct {
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
	RequiredCapabilities []string    `json:"requiredCapabilities"`
	Body                 string      `json:"body"`
	Files                []SkillFile `json:"files"`
}

type SkillFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Executable bool   `json:"executable"`
}

type SkillPackReference struct {
	PackID       string `json:"packId"`
	Commit       string `json:"commit"`
	UpstreamName string `json:"upstreamName"`
}

type Skill struct {
	ID                  string              `json:"id"`
	ScopeID             string              `json:"scopeId"`
	Manifest            SkillManifest       `json:"manifest"`
	Status              string              `json:"status"`
	CreatedBy           string              `json:"createdBy"`
	Version             int                 `json:"version"`
	GrantedCapabilities []string            `json:"grantedCapabilities"`
	Approvals           []string            `json:"approvals"`
	CreatedAt           *int64              `json:"createdAt"`
	UpdatedAt           *int64              `json:"updatedAt"`
	LastUsedAt          *int64              `json:"lastUsedAt"`
	Pack                *SkillPackReference `json:"pack"`
}

type SkillPack struct {
	ID                 string          `json:"id"`
	Kind               string          `json:"kind,omitempty"`
	URL                string          `json:"url"`
	Ref                string          `json:"ref,omitempty"`
	SyncMode           string          `json:"syncMode,omitempty"`
	TrustTier          string          `json:"trustTier,omitempty"`
	Config             json.RawMessage `json:"config,omitempty"`
	TargetScopeID      string          `json:"targetScopeId,omitempty"`
	Subset             json.RawMessage `json:"subset,omitempty"`
	AuthCredentialSlug string          `json:"authCredentialSlug,omitempty"`
	CreatedBy          string          `json:"createdBy,omitempty"`
	CreatedAt          int64           `json:"createdAt,omitempty"`
	LastImport         json.RawMessage `json:"lastImport,omitempty"`
	UpdateAvailable    *bool           `json:"updateAvailable,omitempty"`
	Available          *int            `json:"available,omitempty"`
}

func NewSkillRepository(pg *Postgres) *SkillRepository { return &SkillRepository{pg: pg} }

func (r *SkillRepository) List(ctx context.Context) ([]Skill, error) {
	exists, err := r.tableExists(ctx, "skills")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []Skill{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM skills ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Skill{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var item Skill
		if json.Unmarshal(raw, &item) != nil || item.ScopeID == "" || item.Manifest.Name == "" {
			continue
		}
		if item.ID == "" {
			item.ID = id
		}
		if item.Manifest.RequiredCapabilities == nil {
			item.Manifest.RequiredCapabilities = []string{}
		}
		if item.Manifest.Files == nil {
			item.Manifest.Files = []SkillFile{}
		}
		if item.GrantedCapabilities == nil {
			item.GrantedCapabilities = []string{}
		}
		if item.Approvals == nil {
			item.Approvals = []string{}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SkillRepository) Get(ctx context.Context, id string) (*Skill, error) {
	exists, err := r.tableExists(ctx, "skills")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var raw json.RawMessage
	err = r.pg.Pool.QueryRow(ctx, "SELECT json FROM skills WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var item Skill
	if json.Unmarshal(raw, &item) != nil || item.ScopeID == "" || item.Manifest.Name == "" {
		return nil, nil
	}
	if item.ID == "" {
		item.ID = id
	}
	if item.Manifest.RequiredCapabilities == nil {
		item.Manifest.RequiredCapabilities = []string{}
	}
	if item.Manifest.Files == nil {
		item.Manifest.Files = []SkillFile{}
	}
	if item.GrantedCapabilities == nil {
		item.GrantedCapabilities = []string{}
	}
	if item.Approvals == nil {
		item.Approvals = []string{}
	}
	return &item, nil
}

func (r *SkillRepository) Packs(ctx context.Context) (map[string]SkillPack, error) {
	exists, err := r.tableExists(ctx, "skill_packs")
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[string]SkillPack{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM skill_packs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]SkillPack{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var item SkillPack
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.ID == "" {
			item.ID = id
		}
		result[item.ID] = item
	}
	return result, rows.Err()
}

func (r *SkillRepository) ListPacks(ctx context.Context) ([]SkillPack, error) {
	packs, err := r.Packs(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(packs))
	for id := range packs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]SkillPack, 0, len(ids))
	for _, id := range ids {
		result = append(result, packs[id])
	}
	return result, nil
}

// PatchPack applies Node SkillPackStore.update's DurableMap.merge behavior.
// It deliberately invalidates the shared map version even when the requested
// pack is absent; the HTTP layer turns that missing record into Node's normal
// update error response.
func (r *SkillRepository) PatchPack(ctx context.Context, id string, patch map[string]json.RawMessage, removeKeys []string) (json.RawMessage, bool, error) {
	if strings.TrimSpace(id) == "" {
		return nil, false, errors.New("skill pack id is required")
	}
	if _, err := r.pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS skill_packs(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
		return nil, false, err
	}
	if _, err := r.pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS durable_map_versions(tbl TEXT PRIMARY KEY,v BIGINT NOT NULL)"); err != nil {
		return nil, false, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bumpDurableMapVersion(ctx, tx, "skill_packs"); err != nil {
		return nil, false, err
	}
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM skill_packs WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, false, err
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return nil, false, errors.New("skill pack JSON must be an object")
	}
	for _, key := range removeKeys {
		delete(document, key)
	}
	for key, value := range patch {
		document[key] = value
	}
	next, err := json.Marshal(document)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE skill_packs SET json=$2::jsonb WHERE id=$1", id, string(next)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return next, true, nil
}

// Archive performs the Node SkillStore archive transition and bumps the shared
// durable-map version so Node's short-lived map cache observes the change.
func (r *SkillRepository) Archive(ctx context.Context, id string) (bool, error) {
	exists, err := r.tableExists(ctx, "skills")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM skills WHERE id=$1 FOR UPDATE", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Rollback(ctx)
	}
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return false, errors.New("skill JSON must be an object")
	}
	var status string
	_ = json.Unmarshal(document["status"], &status)
	if status == "archived" {
		return true, tx.Commit(ctx)
	}
	document["status"], _ = json.Marshal("archived")
	document["updatedAt"], _ = json.Marshal(time.Now().UnixMilli())
	next, err := json.Marshal(document)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES('skills',1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE skills SET json=$2::jsonb WHERE id=$1", id, string(next)); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// RemovePack mirrors Node's removeSkillPack durable-map operation. It removes
// only skills materialized by that pack (createdBy=pack:<id>), then discards
// its optional shared bundle and the pack record. The caller is still
// responsible for authorization; Git fetching and pack import stay in Node.
func (r *SkillRepository) RemovePack(ctx context.Context, packID string) (int, error) {
	if strings.TrimSpace(packID) == "" {
		return 0, errors.New("skill pack id is required")
	}
	// Node's PostgreSQL DurableMaps initialize their table and version state on
	// first use, including a delete of an absent key.
	for _, table := range []string{"skills", "skill_packs", "skill_bundles"} {
		if _, err := r.pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+"(id TEXT PRIMARY KEY,json JSONB NOT NULL)"); err != nil {
			return 0, err
		}
	}
	if _, err := r.pg.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS durable_map_versions(tbl TEXT PRIMARY KEY,v BIGINT NOT NULL)"); err != nil {
		return 0, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Share Node's session-level materialization lock key, but bind it to this
	// transaction so cancellation cannot leave a Go-held lock behind.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "skills:materialization"); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, "SELECT id FROM skills WHERE json->>'createdBy'=$1 FOR UPDATE", "pack:"+packID)
	if err != nil {
		return 0, err
	}
	skillIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		skillIDs = append(skillIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range skillIDs {
		if err := bumpDurableMapVersion(ctx, tx, "skills"); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM skills WHERE id=$1", id); err != nil {
			return 0, err
		}
	}
	for _, table := range []string{"skill_bundles", "skill_packs"} {
		if err := bumpDurableMapVersion(ctx, tx, table); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE id=$1", packID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(skillIDs), nil
}

func (r *SkillRepository) tableExists(ctx context.Context, table string) (bool, error) {
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}
