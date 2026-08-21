package data

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// FileArtifact is the metadata shared with Node's PostgreSQL file-artifact
// store. Byte contents remain owned by the configured durable byte store.
type FileArtifact struct {
	ID, OwnerScopeID, CreatedBy, Name, Path, Mimetype, Direction string
	SHA256                                                       string
	SizeBytes, CreatedAt, UpdatedAt                              int64
	BlobKey, CreatedInScope                                      *string
}

// FileArtifactPage is the metadata-only equivalent of Node's FilePage. Blob
// data intentionally remains outside this repository.
type FileArtifactPage struct {
	Files      []FileArtifact
	NextCursor string
}

type FileArtifactRepository struct{ pg *Postgres }

func NewFileArtifactRepository(pg *Postgres) *FileArtifactRepository {
	return &FileArtifactRepository{pg: pg}
}

// Get returns enabled metadata by artifact id. Content bytes remain in the
// configured durable byte store, separate from this PostgreSQL projection.
func (r *FileArtifactRepository) Get(ctx context.Context, id string) (*FileArtifact, error) {
	if id == "" {
		return nil, nil
	}
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return nil, err
	}
	var file FileArtifact
	err = r.pg.Pool.QueryRow(ctx, `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE id=$1 AND enabled=TRUE`, id).Scan(&file.ID, &file.OwnerScopeID, &file.CreatedBy, &file.Name, &file.Path, &file.Mimetype, &file.SizeBytes, &file.BlobKey, &file.SHA256, &file.Direction, &file.CreatedInScope, &file.CreatedAt, &file.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &file, nil
}

// Create persists metadata after a durable byte-store write. The row matches
// Node's file_artifacts schema and remains rebuildable from that shared table.
func (r *FileArtifactRepository) Create(ctx context.Context, file FileArtifact) (*FileArtifact, error) {
	if file.ID == "" || file.OwnerScopeID == "" || file.CreatedBy == "" || file.Name == "" || file.Path == "" || file.Mimetype == "" || file.Direction == "" || file.BlobKey == nil {
		return nil, errors.New("file artifact fields are required")
	}
	if file.CreatedAt <= 0 {
		return nil, errors.New("file artifact created_at is required")
	}
	if file.UpdatedAt <= 0 {
		file.UpdatedAt = file.CreatedAt
	}
	exists, err := r.tableExists(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("file_artifacts table is not available")
	}
	_, err = r.pg.Pool.Exec(ctx, `INSERT INTO file_artifacts(id,kind,owner_scope_id,path,name,mimetype,size_bytes,blob_key,sha256,direction,created_by,created_in_scope,created_at,updated_at,enabled,source)
VALUES($1,'file',$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$13,TRUE,'live')`, file.ID, file.OwnerScopeID, file.Path, file.Name, file.Mimetype, file.SizeBytes, *file.BlobKey, file.SHA256, file.Direction, file.CreatedBy, file.CreatedInScope, file.CreatedAt, file.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &file, nil
}

// NewFileArtifactID produces the same opaque 32-hex shape Node exposes for
// file artifacts. IDs are random while blob keys remain content-addressed.
func NewFileArtifactID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// List returns enabled artifact metadata. The table is created by the Node
// file store only when that capability is wired, so a missing table means an
// empty file collection rather than a control-plane failure.
func (r *FileArtifactRepository) List(ctx context.Context, scopeID string, orgWide bool, limit int) ([]FileArtifact, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	var exists bool
	if err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('file_artifacts') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []FileArtifact{}, nil
	}
	query := `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE enabled=TRUE`
	args := []any{}
	if !orgWide {
		query += " AND owner_scope_id=$1"
		args = append(args, scopeID)
	}
	query += " ORDER BY created_at DESC,id DESC"
	args = append(args, limit)
	query += " LIMIT $" + strconv.Itoa(len(args))
	rows, err := r.pg.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []FileArtifact{}
	for rows.Next() {
		var item FileArtifact
		if err := scanFileArtifact(rows, &item); err != nil {
			return nil, err
		}
		files = append(files, item)
	}
	return files, rows.Err()
}

// ListOwned returns a personal-scope file page for the admin user detail
// response. Unlike List, it uses the Node store's owner scope selector rather
// than an organization-wide administrative filter.
func (r *FileArtifactRepository) ListOwned(ctx context.Context, ownerScopeID string, limit int) ([]FileArtifact, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return []FileArtifact{}, err
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE enabled=TRUE AND owner_scope_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, ownerScopeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileArtifacts(rows)
}

// ListOwnedByScopes mirrors FileArtifactStore.listOwnedByScopes for the
// capability-backed file listing route. Its cursor encoding and ordering match
// the Node store so callers can switch implementations without losing a page.
func (r *FileArtifactRepository) ListOwnedByScopes(ctx context.Context, ownerScopes []string, limit int, cursor, createdInScope string) (FileArtifactPage, error) {
	if len(ownerScopes) == 0 {
		return FileArtifactPage{Files: []FileArtifact{}}, nil
	}
	// A positive fractional Node query value becomes zero after Math.floor.
	// Keep zero intact here so ListOwnedByScopes reproduces that edge case;
	// normal callers pass the Node-compatible default of 50 themselves.
	if limit < 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return FileArtifactPage{Files: []FileArtifact{}}, err
	}
	params := []any{ownerScopes}
	filters := []string{"enabled=TRUE", "owner_scope_id=ANY($1::text[])"}
	if createdInScope != "" {
		params = append(params, createdInScope)
		filters = append(filters, "created_in_scope=$"+strconv.Itoa(len(params))+"::text")
	}
	if at, id, ok := decodeFileCursor(cursor); ok {
		params = append(params, at, id)
		filters = append(filters, "(created_at,id) < ($"+strconv.Itoa(len(params)-1)+"::bigint,$"+strconv.Itoa(len(params))+"::text)")
	}
	params = append(params, limit+1)
	query := `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE ` + strings.Join(filters, " AND ") + ` ORDER BY created_at DESC,id DESC LIMIT $` + strconv.Itoa(len(params))
	rows, err := r.pg.Pool.Query(ctx, query, params...)
	if err != nil {
		return FileArtifactPage{}, err
	}
	defer rows.Close()
	files, err := collectFileArtifacts(rows)
	if err != nil {
		return FileArtifactPage{}, err
	}
	page := FileArtifactPage{Files: files}
	if len(files) > limit {
		page.Files = files[:limit]
		if len(page.Files) > 0 {
			page.NextCursor = encodeFileCursor(page.Files[len(page.Files)-1])
		}
	}
	return page, nil
}

// ListOwnedInScope mirrors FileArtifactStore.listOwnedByScopes with the
// createdInScope selector used by /v1/scope-resources.
func (r *FileArtifactRepository) ListOwnedInScope(ctx context.Context, ownerScopes []string, scopeID string) ([]FileArtifact, error) {
	if len(ownerScopes) == 0 {
		return []FileArtifact{}, nil
	}
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return []FileArtifact{}, err
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE enabled=TRUE AND owner_scope_id=ANY($1::text[]) AND created_in_scope=$2
ORDER BY created_at DESC,id DESC`, ownerScopes, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileArtifacts(rows)
}

// ResolveByOwnerPaths is the durable file-grant lookup used after resolving
// ACL handles for a viewer's resource scopes.
func (r *FileArtifactRepository) ResolveByOwnerPaths(ctx context.Context, handles []FileHandle) ([]FileArtifact, error) {
	if len(handles) == 0 {
		return []FileArtifact{}, nil
	}
	exists, err := r.tableExists(ctx)
	if err != nil || !exists {
		return []FileArtifact{}, err
	}
	owners, paths := make([]string, 0, len(handles)), make([]string, 0, len(handles))
	for _, handle := range handles {
		owners, paths = append(owners, handle.OwnerScopeID), append(paths, handle.Path)
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,owner_scope_id,created_by,name,path,mimetype,size_bytes,blob_key,COALESCE(sha256,''),direction,created_in_scope,created_at,updated_at
FROM file_artifacts WHERE enabled=TRUE AND (owner_scope_id,path) IN (SELECT * FROM unnest($1::text[],$2::text[]))`, owners, paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileArtifacts(rows)
}

func (r *FileArtifactRepository) ListForProject(ctx context.Context, projectID string, limit int) ([]FileArtifact, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT f.id,f.owner_scope_id,f.created_by,f.name,f.path,f.mimetype,f.size_bytes,f.blob_key,COALESCE(f.sha256,''),f.direction,f.created_in_scope,f.created_at,f.updated_at
FROM file_artifacts f JOIN project_file_memberships p ON p.file_id=f.id
WHERE p.project_id=$1 AND f.enabled=TRUE ORDER BY p.created_at DESC,f.id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileArtifacts(rows)
}

type FileHandle struct{ OwnerScopeID, Path string }

type fileArtifactScanner interface{ Scan(...any) error }

func collectFileArtifacts(rows pgx.Rows) ([]FileArtifact, error) {
	files := []FileArtifact{}
	for rows.Next() {
		var item FileArtifact
		if err := scanFileArtifact(rows, &item); err != nil {
			return nil, err
		}
		files = append(files, item)
	}
	return files, rows.Err()
}

func scanFileArtifact(row fileArtifactScanner, item *FileArtifact) error {
	return row.Scan(&item.ID, &item.OwnerScopeID, &item.CreatedBy, &item.Name, &item.Path, &item.Mimetype, &item.SizeBytes, &item.BlobKey, &item.SHA256, &item.Direction, &item.CreatedInScope, &item.CreatedAt, &item.UpdatedAt)
}

func (r *FileArtifactRepository) Delete(ctx context.Context, id string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM file_artifacts WHERE id=$1", id)
	return err
}

func (r *FileArtifactRepository) CountBlobReferences(ctx context.Context, blobKey string) (int, error) {
	var count int
	err := r.pg.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM file_artifacts WHERE blob_key=$1", blobKey).Scan(&count)
	return count, err
}

func (r *FileArtifactRepository) tableExists(ctx context.Context) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass('file_artifacts') IS NOT NULL").Scan(&exists)
	return exists, err
}

func encodeFileCursor(file FileArtifact) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(file.CreatedAt, 10) + "|" + file.ID))
}

func decodeFileCursor(cursor string) (int64, string, bool) {
	if cursor == "" {
		return 0, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(cursor)
	}
	if err != nil {
		return 0, "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) < 2 {
		return 0, "", false
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return at, parts[1], true
}
