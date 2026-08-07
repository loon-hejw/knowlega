package data

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// KeychainStatusRepository exposes the same metadata-only projection that the
// Node admin keychain view reads from its durable maps. It never decrypts or
// returns secretEnc. Credential mutations and materialization stay in Node.
type KeychainStatusRepository struct{ pg *Postgres }

type KeychainStatus struct {
	Enabled     bool             `json:"enabled"`
	Credentials []map[string]any `json:"credentials"`
	Grants      []map[string]any `json:"grants"`
	Asks        []map[string]any `json:"asks"`
}

// ConnectorTokenStatus is the metadata-only status of one deterministic OAuth
// credential slot. It is deliberately sufficient for status displays but
// excludes both access-token and refresh-token ciphertext.
type ConnectorTokenStatus struct {
	OwnerID         string
	Host            string
	AccountType     string
	Connected       bool
	ExpiresAt       *int64
	HasRefreshToken bool
	NeedsReconnect  bool
	RefreshFailedAt *int64
	RefreshError    string
	GrantedScopes   []string
}

// KeychainGrantInput is the persistence-only direct-grant path. Ask approval
// remains a Node runtime operation because it resumes a waiting turn; a direct
// owner grant only updates durable maps and can therefore move independently.
type KeychainGrantInput struct {
	CredentialID    string
	OwnerID         string
	AudienceScopeID string
	Mode            string
	Purpose         string
	ExpiresAt       *int64
}

type KeychainGrantResult struct {
	Grant       map[string]any
	AdoptedAsks []map[string]any
}

type KeychainGrantError struct {
	Status  int
	Message string
}

func (e *KeychainGrantError) Error() string { return e.Message }

func NewKeychainStatusRepository(pg *Postgres) *KeychainStatusRepository {
	return &KeychainStatusRepository{pg: pg}
}

// CreateDirectGrant mirrors Keychain.createGrant followed by
// resolveAsksForGrant. It intentionally does not fire an ask-resolution turn:
// Node's direct-grant route marks matching asks notified but does not enqueue
// that runtime callback either.
func (r *KeychainStatusRepository) CreateDirectGrant(ctx context.Context, input KeychainGrantInput) (KeychainGrantResult, error) {
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS keychain_credentials(id TEXT PRIMARY KEY,json JSONB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS keychain_grants(id TEXT PRIMARY KEY,json JSONB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS keychain_asks(id TEXT PRIMARY KEY,json JSONB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS durable_map_versions(tbl TEXT PRIMARY KEY,v BIGINT NOT NULL)",
	} {
		if _, err := r.pg.Pool.Exec(ctx, statement); err != nil {
			return KeychainGrantResult{}, err
		}
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return KeychainGrantResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var credentialRaw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM keychain_credentials WHERE id=$1 FOR UPDATE", input.CredentialID).Scan(&credentialRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeychainGrantResult{}, &KeychainGrantError{Status: 404, Message: "unknown credential"}
	}
	if err != nil {
		return KeychainGrantResult{}, err
	}
	credential := map[string]any{}
	if json.Unmarshal(credentialRaw, &credential) != nil {
		return KeychainGrantResult{}, errors.New("credential JSON must be an object")
	}
	if stringField(credential, "kind") == "broker" {
		return KeychainGrantResult{}, &KeychainGrantError{Status: 400, Message: "broker credentials are org-owned and used via the credential broker, not grants"}
	}
	if !sameKeychainOwner(stringField(credential, "ownerId"), input.OwnerID) {
		return KeychainGrantResult{}, &KeychainGrantError{Status: 403, Message: "only the credential's owner can grant it — and only on a turn the owner themself sent"}
	}
	purpose := strings.TrimSpace(input.Purpose)
	if purpose == "" {
		return KeychainGrantResult{}, &KeychainGrantError{Status: 400, Message: "purpose required — record the owner's approval verbatim"}
	}
	now := time.Now().UnixMilli()
	if stringField(credential, "managed") == "" && stringField(credential, "kind") != "file" {
		if expiresAt, hasExpiry := numericField(credential, "expiresAt"); hasExpiry && expiresAt < now {
			return KeychainGrantResult{}, &KeychainGrantError{Status: 410, Message: "credential is expired"}
		}
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{input.CredentialID, input.AudienceScopeID, strconv.FormatInt(now, 10), purpose}, "\x00")))
	grant := map[string]any{
		"id":              fmt.Sprintf("%x", digest[:])[:16],
		"credentialId":    input.CredentialID,
		"ownerId":         stringField(credential, "ownerId"),
		"audienceScopeId": input.AudienceScopeID,
		"mode":            input.Mode,
		"purpose":         purpose,
		"status":          "active",
		"createdAt":       now,
	}
	if orgID := stringField(credential, "orgId"); orgID != "" {
		grant["orgId"] = orgID
	}
	if input.ExpiresAt != nil {
		grant["expiresAt"] = *input.ExpiresAt
	}
	if err := putKeychainGrant(ctx, tx, grant); err != nil {
		return KeychainGrantResult{}, err
	}
	if err := bumpDurableMapVersion(ctx, tx, "keychain_grants"); err != nil {
		return KeychainGrantResult{}, err
	}

	rows, err := tx.Query(ctx, "SELECT id,json FROM keychain_asks ORDER BY id FOR UPDATE")
	if err != nil {
		return KeychainGrantResult{}, err
	}
	defer rows.Close()
	type askUpdate struct {
		id       string
		document map[string]any
		adopted  bool
	}
	updates := []askUpdate{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return KeychainGrantResult{}, err
		}
		ask := map[string]any{}
		if json.Unmarshal(raw, &ask) != nil {
			continue
		}
		if stringField(ask, "status") == "pending" && numberField(ask, "expiresAt") < now {
			ask["status"], ask["resolvedAt"] = "expired", now
			updates = append(updates, askUpdate{id: id, document: ask})
			continue
		}
		if stringField(ask, "status") == "pending" && stringField(ask, "credentialId") == input.CredentialID && stringField(ask, "requesterScopeId") == input.AudienceScopeID {
			ask["status"], ask["resolvedAt"], ask["grantId"], ask["notifiedAt"] = "approved", now, grant["id"], now
			updates = append(updates, askUpdate{id: id, document: ask, adopted: true})
		}
	}
	if err := rows.Err(); err != nil {
		return KeychainGrantResult{}, err
	}
	rows.Close()
	result := KeychainGrantResult{Grant: grant, AdoptedAsks: []map[string]any{}}
	for _, update := range updates {
		encoded, err := json.Marshal(update.document)
		if err != nil {
			return KeychainGrantResult{}, err
		}
		if _, err := tx.Exec(ctx, "UPDATE keychain_asks SET json=$2::jsonb WHERE id=$1", update.id, encoded); err != nil {
			return KeychainGrantResult{}, err
		}
		if err := bumpDurableMapVersion(ctx, tx, "keychain_asks"); err != nil {
			return KeychainGrantResult{}, err
		}
		if !update.adopted {
			continue
		}
		grant["askId"] = update.id
		if err := putKeychainGrant(ctx, tx, grant); err != nil {
			return KeychainGrantResult{}, err
		}
		if err := bumpDurableMapVersion(ctx, tx, "keychain_grants"); err != nil {
			return KeychainGrantResult{}, err
		}
		result.AdoptedAsks = append(result.AdoptedAsks, update.document)
	}
	result.Grant = grant
	if err := tx.Commit(ctx); err != nil {
		return KeychainGrantResult{}, err
	}
	return result, nil
}

func putKeychainGrant(ctx context.Context, tx pgx.Tx, grant map[string]any) error {
	id := stringField(grant, "id")
	payload, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO keychain_grants(id,json) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET json=EXCLUDED.json", id, payload)
	return err
}

func (r *KeychainStatusRepository) List(ctx context.Context) (KeychainStatus, error) {
	// Each Node DurableMap is created on first use. Credentials may therefore
	// exist while asks have never been opened; do not let one absent optional
	// map hide data from another initialized map.
	credentials, err := r.documentsIfExists(ctx, "keychain_credentials")
	if err != nil {
		return KeychainStatus{}, err
	}
	grants, err := r.documentsIfExists(ctx, "keychain_grants")
	if err != nil {
		return KeychainStatus{}, err
	}
	asks, err := r.freshAsks(ctx)
	if err != nil {
		return KeychainStatus{}, err
	}

	metadata := make([]map[string]any, 0, len(credentials))
	for _, credential := range credentials {
		// listAllMetadata excludes managed connector credentials and service
		// broker entries. It strips only the primary encrypted secret, exactly
		// as Node's toMeta projection does.
		if stringField(credential, "managed") != "" || stringField(credential, "kind") == "broker" {
			continue
		}
		delete(credential, "secretEnc")
		metadata = append(metadata, credential)
	}
	return KeychainStatus{Enabled: true, Credentials: metadata, Grants: grants, Asks: asks}, nil
}

// ConnectorMetadata returns the safe status projection for managed OAuth
// credentials. It mirrors Node's connectorMeta helper and never returns the
// token ciphertext or refresh-token ciphertext embedded in the durable row.
func (r *KeychainStatusRepository) ConnectorMetadata(ctx context.Context) ([]map[string]any, error) {
	exists, err := r.tableExists(ctx, "keychain_credentials")
	if err != nil || !exists {
		return []map[string]any{}, err
	}
	credentials, err := r.documents(ctx, "keychain_credentials")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	result := make([]map[string]any, 0)
	for _, credential := range credentials {
		if stringField(credential, "managed") != "connector" {
			continue
		}
		refresh, _ := credential["refresh"].(map[string]any)
		expiresAt := numberField(credential, "expiresAt")
		hasExpiresAt := expiresAt != 0
		hasRefresh := stringField(refresh, "refreshTokenEnc") != ""
		refreshFailed := numberField(refresh, "refreshFailedAt") != 0
		item := map[string]any{
			"credentialId": stringField(credential, "id"),
			"ownerId":      stringField(credential, "ownerId"),
			"host":         firstKey(credential, "host", "service"),
			"connected":    true,
		}
		if accountType := stringField(refresh, "accountType"); accountType != "" {
			item["accountType"] = accountType
		}
		if hasExpiresAt {
			item["expiresAt"] = expiresAt
		}
		if hasExpiresAt && now >= expiresAt-60_000 && (!hasRefresh || refreshFailed) {
			item["needsReconnect"] = true
		}
		result = append(result, item)
	}
	return result, nil
}

// ConnectorTokenStatuses mirrors Node's connectorTokenStatus projection for
// every managed connector credential owned by principalID. It intentionally
// reads no encrypted fields, and does not attempt an OAuth refresh; Node keeps
// that runtime operation while Go serves the safe control-plane status view.
func (r *KeychainStatusRepository) ConnectorTokenStatuses(ctx context.Context, principalID string) ([]ConnectorTokenStatus, error) {
	exists, err := r.tableExists(ctx, "keychain_credentials")
	if err != nil || !exists {
		return []ConnectorTokenStatus{}, err
	}
	credentials, err := r.documents(ctx, "keychain_credentials")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	statuses := make([]ConnectorTokenStatus, 0)
	for _, credential := range credentials {
		ownerID := stringField(credential, "ownerId")
		if stringField(credential, "managed") != "connector" || !sameKeychainOwner(ownerID, principalID) {
			continue
		}
		refresh, _ := credential["refresh"].(map[string]any)
		expiresAt, hasExpiresAt := numericField(credential, "expiresAt")
		hasRefresh := stringField(refresh, "refreshTokenEnc") != ""
		refreshFailedAt, refreshFailed := numericField(refresh, "refreshFailedAt")
		status := ConnectorTokenStatus{
			OwnerID:         ownerID,
			Host:            firstKey(credential, "host", "service"),
			AccountType:     stringField(refresh, "accountType"),
			Connected:       true,
			HasRefreshToken: hasRefresh,
		}
		if hasExpiresAt {
			status.ExpiresAt = &expiresAt
			status.NeedsReconnect = now >= expiresAt-60_000 && (!hasRefresh || refreshFailed)
		}
		if refreshFailed {
			status.RefreshFailedAt = &refreshFailedAt
			status.RefreshError = stringField(refresh, "refreshError")
		}
		if rawScopes, ok := refresh["grantedScopes"].([]any); ok {
			for _, rawScope := range rawScopes {
				if scope, ok := rawScope.(string); ok {
					status.GrantedScopes = append(status.GrantedScopes, scope)
				}
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// DeleteOwnedCredential mirrors Node's keychain.remove operation. It never
// reads plaintext: active grants are marked revoked and the safe-to-delete
// credential row is removed in one transaction with map-version invalidation.
func (r *KeychainStatusRepository) DeleteOwnedCredential(ctx context.Context, ownerID, credentialID string) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM keychain_credentials WHERE id=$1 FOR UPDATE", credentialID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	credential := map[string]any{}
	if json.Unmarshal(raw, &credential) != nil || !sameKeychainOwner(stringField(credential, "ownerId"), ownerID) || stringField(credential, "managed") != "" || stringField(credential, "kind") == "broker" {
		return false, nil
	}
	rows, err := tx.Query(ctx, "SELECT id,json FROM keychain_grants FOR UPDATE")
	if err != nil {
		return false, err
	}
	grantsToRevoke := make([]struct {
		id   string
		json []byte
	}, 0)
	now := time.Now().UnixMilli()
	for rows.Next() {
		var id string
		var grantRaw json.RawMessage
		if err := rows.Scan(&id, &grantRaw); err != nil {
			rows.Close()
			return false, err
		}
		grant := map[string]any{}
		if json.Unmarshal(grantRaw, &grant) != nil || stringField(grant, "credentialId") != credentialID || stringField(grant, "status") != "active" {
			continue
		}
		grant["status"], grant["revokedAt"] = "revoked", now
		updated, err := json.Marshal(grant)
		if err != nil {
			rows.Close()
			return false, err
		}
		grantsToRevoke = append(grantsToRevoke, struct {
			id   string
			json []byte
		}{id: id, json: updated})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, grant := range grantsToRevoke {
		if _, err := tx.Exec(ctx, "UPDATE keychain_grants SET json=$2::jsonb WHERE id=$1", grant.id, grant.json); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM keychain_credentials WHERE id=$1", credentialID); err != nil {
		return false, err
	}
	for range grantsToRevoke {
		if err := bumpDurableMapVersion(ctx, tx, "keychain_grants"); err != nil {
			return false, err
		}
	}
	if err := bumpDurableMapVersion(ctx, tx, "keychain_credentials"); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// RevokeOwnedGrant matches Node's revokeGrant: any grant owned by the caller
// is acknowledged, while only an active record mutates and invalidates the
// shared grants durable-map version.
func (r *KeychainStatusRepository) RevokeOwnedGrant(ctx context.Context, ownerID, grantID string) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM keychain_grants WHERE id=$1 FOR UPDATE", grantID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	grant := map[string]any{}
	if json.Unmarshal(raw, &grant) != nil || !sameKeychainOwner(stringField(grant, "ownerId"), ownerID) {
		return false, nil
	}
	if stringField(grant, "status") == "active" {
		grant["status"], grant["revokedAt"] = "revoked", time.Now().UnixMilli()
		updated, err := json.Marshal(grant)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, "UPDATE keychain_grants SET json=$2::jsonb WHERE id=$1", grantID, updated); err != nil {
			return false, err
		}
		if err := bumpDurableMapVersion(ctx, tx, "keychain_grants"); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// DeclineAsk mirrors Keychain.declineAsk's durable transition. It deliberately
// leaves notifiedAt unset: the existing Node ask sweep observes that resolved,
// unnotified record and remains responsible for resuming the waiting turn or
// sending its fallback delivery.
func (r *KeychainStatusRepository) DeclineAsk(ctx context.Context, ownerID, askID, note string) (map[string]any, error) {
	exists, err := r.tableExists(ctx, "keychain_asks")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &KeychainGrantError{Status: 404, Message: "unknown ask"}
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM keychain_asks WHERE id=$1 FOR UPDATE", askID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &KeychainGrantError{Status: 404, Message: "unknown ask"}
	}
	if err != nil {
		return nil, err
	}
	ask := map[string]any{}
	if json.Unmarshal(raw, &ask) != nil || ask == nil {
		return nil, errors.New("ask JSON must be an object")
	}
	if !sameKeychainOwner(stringField(ask, "ownerId"), ownerID) {
		return nil, &KeychainGrantError{Status: 403, Message: "only the credential's owner can decline an ask"}
	}
	now := time.Now().UnixMilli()
	if stringField(ask, "status") == "pending" && numberField(ask, "expiresAt") < now {
		ask["status"], ask["resolvedAt"] = "expired", now
		if err := updateKeychainAsk(ctx, tx, askID, ask); err != nil {
			return nil, err
		}
		if err := bumpDurableMapVersion(ctx, tx, "keychain_asks"); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, &KeychainGrantError{Status: 410, Message: "ask already expired"}
	}
	if status := stringField(ask, "status"); status != "pending" {
		return nil, &KeychainGrantError{Status: 410, Message: "ask already " + status}
	}
	ask["status"], ask["resolvedAt"] = "declined", now
	if note = strings.TrimSpace(note); note != "" {
		ask["note"] = note
	}
	if err := updateKeychainAsk(ctx, tx, askID, ask); err != nil {
		return nil, err
	}
	if err := bumpDurableMapVersion(ctx, tx, "keychain_asks"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ask, nil
}

func updateKeychainAsk(ctx context.Context, tx pgx.Tx, id string, ask map[string]any) error {
	payload, err := json.Marshal(ask)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE keychain_asks SET json=$2::jsonb WHERE id=$1", id, payload)
	return err
}

// DeleteConnectorTokens mirrors Node's connector token-store deletion. OAuth
// credentials use deterministic IDs rather than a secret lookup, so this
// operation never decrypts their contents. A call covers the default,
// personal, and company account slots for every host. Like Node's DurableMap
// implementation, every delete attempt invalidates the credential map, even
// when the slot was already absent; active grants are revoked individually.
// Authorization of principalID is deliberately enforced by the HTTP layer.
func (r *KeychainStatusRepository) DeleteConnectorTokens(ctx context.Context, principalID string, hosts []string) error {
	// Node's Postgres DurableMap lazily creates the credentials and grants maps
	// on the first connector deletion. Go must do the same so enabling this
	// route before a Node worker has touched keychain state does not turn a
	// valid disconnect into a database-table error.
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS keychain_credentials(id TEXT PRIMARY KEY,json JSONB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS keychain_grants(id TEXT PRIMARY KEY,json JSONB NOT NULL)",
		"CREATE TABLE IF NOT EXISTS durable_map_versions(tbl TEXT PRIMARY KEY,v BIGINT NOT NULL)",
	} {
		if _, err := r.pg.Pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, host := range hosts {
		for _, accountType := range []string{"", "personal", "company"} {
			credentialID := connectorCredentialID(principalID, host, accountType)
			rows, err := tx.Query(ctx, "SELECT id,json FROM keychain_grants WHERE json->>'credentialId'=$1 FOR UPDATE", credentialID)
			if err != nil {
				return err
			}
			grantsToRevoke := make([]struct {
				id   string
				json []byte
			}, 0)
			now := time.Now().UnixMilli()
			for rows.Next() {
				var id string
				var raw json.RawMessage
				if err := rows.Scan(&id, &raw); err != nil {
					rows.Close()
					return err
				}
				grant := map[string]any{}
				if json.Unmarshal(raw, &grant) != nil || stringField(grant, "status") != "active" {
					continue
				}
				grant["status"], grant["revokedAt"] = "revoked", now
				updated, err := json.Marshal(grant)
				if err != nil {
					rows.Close()
					return err
				}
				grantsToRevoke = append(grantsToRevoke, struct {
					id   string
					json []byte
				}{id: id, json: updated})
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			for _, grant := range grantsToRevoke {
				if _, err := tx.Exec(ctx, "UPDATE keychain_grants SET json=$2::jsonb WHERE id=$1", grant.id, grant.json); err != nil {
					return err
				}
				if err := bumpDurableMapVersion(ctx, tx, "keychain_grants"); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, "DELETE FROM keychain_credentials WHERE id=$1", credentialID); err != nil {
				return err
			}
			if err := bumpDurableMapVersion(ctx, tx, "keychain_credentials"); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func connectorCredentialID(principalID, host, accountType string) string {
	slot := "oauth:"
	if accountType != "" && accountType != "default" {
		slot += accountType
	}
	digest := sha256.Sum256([]byte(principalID + "\x00" + strings.ToLower(host) + "\x00" + slot))
	return fmt.Sprintf("%x", digest[:])[:16]
}

func (r *KeychainStatusRepository) freshAsks(ctx context.Context) ([]map[string]any, error) {
	items, err := r.documentsIfExists(ctx, "keychain_asks")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, item := range items {
		if stringField(item, "status") != "pending" || numberField(item, "expiresAt") >= now {
			continue
		}
		id := stringField(item, "id")
		if id == "" {
			continue
		}
		if err := r.expireAsk(ctx, id, now); err != nil {
			return nil, err
		}
		item["status"], item["resolvedAt"] = "expired", now
	}
	sort.Slice(items, func(i, j int) bool {
		return numberField(items[i], "createdAt") < numberField(items[j], "createdAt")
	})
	return items, nil
}

func (r *KeychainStatusRepository) expireAsk(ctx context.Context, id string, now int64) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw json.RawMessage
	if err := tx.QueryRow(ctx, "SELECT json FROM keychain_asks WHERE id=$1 FOR UPDATE", id).Scan(&raw); err != nil {
		return err
	}
	item := map[string]any{}
	if json.Unmarshal(raw, &item) != nil || stringField(item, "status") != "pending" || numberField(item, "expiresAt") >= now {
		return tx.Commit(ctx)
	}
	item["status"], item["resolvedAt"] = "expired", now
	updated, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE keychain_asks SET json=$2::jsonb WHERE id=$1", id, updated); err != nil {
		return err
	}
	// Node's DurableMap.merge invalidates its versioned local cache within the
	// same mutation boundary, so the reader cannot observe a changed ask with
	// the old map version.
	if err := bumpDurableMapVersion(ctx, tx, "keychain_asks"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func bumpDurableMapVersion(ctx context.Context, tx pgx.Tx, table string) error {
	_, err := tx.Exec(ctx, `INSERT INTO durable_map_versions(tbl,v) VALUES($1,1)
ON CONFLICT(tbl) DO UPDATE SET v=durable_map_versions.v+1`, table)
	return err
}

func (r *KeychainStatusRepository) documents(ctx context.Context, table string) ([]map[string]any, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM "+table+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		item := map[string]any{}
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		if stringField(item, "id") == "" {
			item["id"] = id
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *KeychainStatusRepository) documentsIfExists(ctx context.Context, table string) ([]map[string]any, error) {
	exists, err := r.tableExists(ctx, table)
	if err != nil || !exists {
		return []map[string]any{}, err
	}
	return r.documents(ctx, table)
}

func (r *KeychainStatusRepository) tableExists(ctx context.Context, table string) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	return exists, err
}

func stringField(item map[string]any, key string) string {
	value, _ := item[key].(string)
	return value
}

func numberField(item map[string]any, key string) int64 {
	value, _ := numericField(item, key)
	return value
}

func numericField(item map[string]any, key string) (int64, bool) {
	switch value := item[key].(type) {
	case float64:
		return int64(value), true
	case int64:
		return value, true
	case json.Number:
		parsed, _ := value.Int64()
		return parsed, true
	default:
		return 0, false
	}
}

func firstKey(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(item, key); value != "" {
			return value
		}
	}
	return ""
}

func sameKeychainOwner(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if strings.Contains(left, "@") && strings.Contains(right, "@") {
		return strings.EqualFold(left, right)
	}
	return left == right
}
