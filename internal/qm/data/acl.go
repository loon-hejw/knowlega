package data

import (
	"context"
	"errors"
)

type Grant struct {
	OwnerScopeID   string `json:"ownerScopeId"`
	Path           string `json:"ref"`
	GranteeScopeID string `json:"granteeScopeId"`
	Permission     string `json:"permission"`
	GrantedBy      string `json:"grantedBy"`
}

type AdminGrant struct {
	PrincipalID string `json:"principalId"`
	ScopeID     string `json:"scopeId"`
	Role        string `json:"role"`
	GrantedBy   string `json:"grantedBy,omitempty"`
	CreatedAt   int64  `json:"createdAt,omitempty"`
}

type ACLRepository struct{ pg *Postgres }

func NewACLRepository(pg *Postgres) *ACLRepository { return &ACLRepository{pg: pg} }

func (r *ACLRepository) ReplaceResource(ctx context.Context, ownerScopeID, path string, expected, replacement []Grant) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "acl-grants:"+ownerScopeID+"\n"+path); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT owner_scope_id,path,grantee_scope_id,permission,granted_by FROM acl_grants WHERE owner_scope_id=$1 AND path=$2 FOR UPDATE`, ownerScopeID, path)
	if err != nil {
		return false, err
	}
	current := []Grant{}
	for rows.Next() {
		var grant Grant
		if err := rows.Scan(&grant.OwnerScopeID, &grant.Path, &grant.GranteeScopeID, &grant.Permission, &grant.GrantedBy); err != nil {
			rows.Close()
			return false, err
		}
		current = append(current, grant)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if !sameGrants(current, expected) {
		return false, nil
	}
	if _, err = tx.Exec(ctx, "DELETE FROM acl_grants WHERE owner_scope_id=$1 AND path=$2", ownerScopeID, path); err != nil {
		return false, err
	}
	for _, grant := range replacement {
		if grant.OwnerScopeID != ownerScopeID || grant.Path != path || (grant.Permission != "read" && grant.Permission != "write") {
			return false, errors.New("invalid ACL grant")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO acl_grants(owner_scope_id,path,grantee_scope_id,permission,granted_by) VALUES($1,$2,$3,$4,$5)`, grant.OwnerScopeID, grant.Path, grant.GranteeScopeID, grant.Permission, grant.GrantedBy); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (r *ACLRepository) List(ctx context.Context, ownerScopeID, path string) ([]Grant, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT owner_scope_id,path,grantee_scope_id,permission,granted_by FROM acl_grants WHERE owner_scope_id=$1 AND path=$2 ORDER BY grantee_scope_id,permission`, ownerScopeID, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []Grant{}
	for rows.Next() {
		var grant Grant
		if err := rows.Scan(&grant.OwnerScopeID, &grant.Path, &grant.GranteeScopeID, &grant.Permission, &grant.GrantedBy); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// Put is Node's grant-persistence insert: duplicate grants are idempotent.
func (r *ACLRepository) Put(ctx context.Context, grant Grant) error {
	if grant.Permission != "read" && grant.Permission != "write" {
		return errors.New("invalid ACL grant")
	}
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO acl_grants(owner_scope_id,path,grantee_scope_id,permission,granted_by)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(owner_scope_id,path,grantee_scope_id,permission) DO NOTHING`, grant.OwnerScopeID, grant.Path, grant.GranteeScopeID, grant.Permission, grant.GrantedBy)
	return err
}

// Revoke removes every permission variant for one principal/scope handle,
// matching the Node ACL store's revoke behavior.
func (r *ACLRepository) Revoke(ctx context.Context, ownerScopeID, path, granteeScopeID string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM acl_grants WHERE owner_scope_id=$1 AND path=$2 AND grantee_scope_id=$3", ownerScopeID, path, granteeScopeID)
	return err
}

func (r *ACLRepository) DeleteResource(ctx context.Context, ownerScopeID, path string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM acl_grants WHERE owner_scope_id=$1 AND path=$2", ownerScopeID, path)
	return err
}

// FileHandlesFor implements the Node ACL store's handlesFor projection. A
// resource path with no known typed-resource prefix is a file path.
func (r *ACLRepository) FileHandlesFor(ctx context.Context, scopes []string) ([]FileHandle, error) {
	if len(scopes) == 0 {
		return []FileHandle{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT DISTINCT owner_scope_id,path FROM acl_grants
WHERE grantee_scope_id=ANY($1::text[])
  AND path NOT LIKE 'skill:%' AND path NOT LIKE 'deployment:%'
  AND path NOT LIKE 'cron:%' AND path NOT LIKE 'service-cred:%'
ORDER BY owner_scope_id,path`, scopes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	handles := []FileHandle{}
	for rows.Next() {
		var handle FileHandle
		if err := rows.Scan(&handle.OwnerScopeID, &handle.Path); err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	return handles, rows.Err()
}

func (r *ACLRepository) PutAdminGrant(ctx context.Context, grant AdminGrant) error {
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO admin_grants(principal_id,scope_id,role,granted_by,created_at) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5::bigint,0))
ON CONFLICT(principal_id,scope_id,role) DO UPDATE SET granted_by=EXCLUDED.granted_by,created_at=EXCLUDED.created_at`, grant.PrincipalID, grant.ScopeID, grant.Role, grant.GrantedBy, grant.CreatedAt)
	return err
}

func (r *ACLRepository) DeleteAdminGrant(ctx context.Context, principalID, scopeID, role string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM admin_grants WHERE principal_id=$1 AND scope_id=$2 AND role=$3", principalID, scopeID, role)
	return err
}

func (r *ACLRepository) ListAdminGrants(ctx context.Context, principalID string) ([]AdminGrant, error) {
	query := `SELECT principal_id,scope_id,role,granted_by,created_at FROM admin_grants ORDER BY principal_id,scope_id,role`
	args := []any{}
	if principalID != "" {
		query = `SELECT principal_id,scope_id,role,granted_by,created_at FROM admin_grants WHERE principal_id=$1 ORDER BY scope_id,role`
		args = append(args, principalID)
	}
	rows, err := r.pg.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []AdminGrant{}
	for rows.Next() {
		var grant AdminGrant
		var grantedBy *string
		var createdAt *int64
		if err := rows.Scan(&grant.PrincipalID, &grant.ScopeID, &grant.Role, &grantedBy, &createdAt); err != nil {
			return nil, err
		}
		if grantedBy != nil {
			grant.GrantedBy = *grantedBy
		}
		if createdAt != nil {
			grant.CreatedAt = *createdAt
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func sameGrants(left, right []Grant) bool {
	if len(left) != len(right) {
		return false
	}
	for _, candidate := range left {
		found := false
		for _, expected := range right {
			if candidate == expected {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
