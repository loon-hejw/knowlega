package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const SecretDropTTL = 7 * 24 * time.Hour

type SecretDropField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	IsSecret bool   `json:"isSecret"`
}

type SecretDrop struct {
	ID              string            `json:"id"`
	OwnerID         string            `json:"ownerId"`
	OrgID           string            `json:"orgId"`
	Service         string            `json:"service"`
	EnvKey          string            `json:"envKey,omitempty"`
	Host            string            `json:"host,omitempty"`
	Fields          []SecretDropField `json:"fields"`
	Purpose         string            `json:"purpose"`
	RequestedBy     string            `json:"requestedBy"`
	AudienceScopeID string            `json:"audienceScopeId"`
	ScopeVersion    string            `json:"scopeVersion,omitempty"`
	GrantMode       string            `json:"grantMode"`
	Destination     string            `json:"destination,omitempty"`
	ThreadRef       string            `json:"threadRef,omitempty"`
	RequiresToken   bool              `json:"requiresToken"`
	CreatedAt       int64             `json:"createdAt"`
}

type SecretDropRepository struct{ pg *Postgres }

func NewSecretDropRepository(pg *Postgres) *SecretDropRepository {
	return &SecretDropRepository{pg: pg}
}

func (r *SecretDropRepository) Mint(ctx context.Context, drop SecretDrop) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	payload, err := json.Marshal(drop)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `INSERT INTO secret_drops(id, json) VALUES($1, $2)`, drop.ID, payload)
	if err != nil {
		return err
	}

	if err := bumpDurableMapVersion(ctx, tx, "secret_drops"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (r *SecretDropRepository) Peek(ctx context.Context, dropID string) (*SecretDrop, error) {
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM secret_drops WHERE id=$1", dropID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var drop SecretDrop
	if err := json.Unmarshal(raw, &drop); err != nil {
		return nil, err
	}

	if time.Since(time.Unix(drop.CreatedAt, 0)) > SecretDropTTL {
		return nil, nil
	}

	return &drop, nil
}

func (r *SecretDropRepository) Redeem(ctx context.Context, dropID string) (*SecretDrop, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var raw json.RawMessage
	err = tx.QueryRow(ctx, "SELECT json FROM secret_drops WHERE id=$1", dropID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var drop SecretDrop
	if err := json.Unmarshal(raw, &drop); err != nil {
		return nil, err
	}

	if time.Since(time.Unix(drop.CreatedAt, 0)) > SecretDropTTL {
		return nil, nil
	}

	if drop.GrantMode == "once" {
		_, err = tx.Exec(ctx, `DELETE FROM secret_drops WHERE id=$1`, dropID)
		if err != nil {
			return nil, err
		}

		if err := bumpDurableMapVersion(ctx, tx, "secret_drops"); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &drop, nil
}

type Credential struct {
	ID          string          `json:"id"`
	OwnerID     string          `json:"ownerId"`
	OrgID       string          `json:"orgId"`
	Service     string          `json:"service"`
	Key         string          `json:"key"`
	Values      json.RawMessage `json:"values"`
	GrantedBy   string          `json:"grantedBy"`
	GrantedAt   int64           `json:"grantedAt"`
	Destination string          `json:"destination,omitempty"`
}

func (r *KeychainStatusRepository) CreateCredential(ctx context.Context, cred Credential) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	payload, err := json.Marshal(cred)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `INSERT INTO keychain_credentials(id, json) VALUES($1, $2)
		ON CONFLICT(id) DO UPDATE SET json=$2`, cred.ID, payload)
	if err != nil {
		return err
	}

	if err := bumpDurableMapVersion(ctx, tx, "keychain_credentials"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
