package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type ChannelPolicy struct {
	Container      string         `json:"container"`
	Orders         string         `json:"orders"`
	Bots           map[string]any `json:"bots"`
	AmbientEnabled *bool          `json:"ambientEnabled,omitempty"`
	SetBy          *string        `json:"setBy,omitempty"`
	UpdatedAt      int64          `json:"updatedAt"`
}

type ChannelPolicyRepository struct {
	pg    *Postgres
	orgID string
	now   func() time.Time
}

type ChannelPolicySetOptions struct {
	Bots              *map[string]any
	AmbientEnabled    *bool
	SetAmbientEnabled bool
	ExpectedUpdatedAt *int64
}

func NewChannelPolicyRepository(pg *Postgres, orgID string) *ChannelPolicyRepository {
	return &ChannelPolicyRepository{pg: pg, orgID: orgID, now: time.Now}
}

func (r *ChannelPolicyRepository) Get(ctx context.Context, container string) (*ChannelPolicy, error) {
	return scanChannelPolicy(r.pg.Pool.QueryRow(ctx, "SELECT container,orders,bots,ambient_enabled,set_by,updated_at FROM channel_policy WHERE org_id=$1 AND container=$2", r.orgID, strings.TrimSpace(container)))
}

func (r *ChannelPolicyRepository) Set(ctx context.Context, container, orders, setBy string) (*ChannelPolicy, error) {
	policy, conflict, err := r.SetWithOptions(ctx, container, orders, setBy, ChannelPolicySetOptions{})
	if err != nil {
		return nil, err
	}
	if conflict {
		return nil, errors.New("channel policy conflict")
	}
	return policy, nil
}

func (r *ChannelPolicyRepository) SetWithOptions(ctx context.Context, container, orders, setBy string, options ChannelPolicySetOptions) (*ChannelPolicy, bool, error) {
	container = strings.TrimSpace(container)
	if container == "" {
		return nil, false, errors.New("container required")
	}
	at := r.now().UnixMilli()
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanChannelPolicy(tx.QueryRow(ctx, "SELECT container,orders,bots,ambient_enabled,set_by,updated_at FROM channel_policy WHERE org_id=$1 AND container=$2 FOR UPDATE", r.orgID, container))
	if err != nil {
		return nil, false, err
	}
	if options.ExpectedUpdatedAt != nil && (current == nil && *options.ExpectedUpdatedAt != 0 || current != nil && current.UpdatedAt != *options.ExpectedUpdatedAt) {
		return nil, true, nil
	}
	bots := map[string]any{}
	if current != nil {
		bots = current.Bots
	}
	if options.Bots != nil {
		bots = *options.Bots
	}
	ambient := (*bool)(nil)
	if current != nil {
		ambient = current.AmbientEnabled
	}
	if options.SetAmbientEnabled {
		ambient = options.AmbientEnabled
	}
	encodedBots, err := json.Marshal(bots)
	if err != nil {
		return nil, false, err
	}
	var policy *ChannelPolicy
	if current == nil {
		policy, err = scanChannelPolicy(tx.QueryRow(ctx, `INSERT INTO channel_policy(org_id,container,orders,bots,ambient_enabled,set_by,updated_at)
VALUES($1,$2,$3,$4::jsonb,$5,NULLIF($6,''),$7)
RETURNING container,orders,bots,ambient_enabled,set_by,updated_at`, r.orgID, container, orders, string(encodedBots), ambient, setBy, at))
	} else {
		policy, err = scanChannelPolicy(tx.QueryRow(ctx, `UPDATE channel_policy SET orders=$3,bots=$4::jsonb,ambient_enabled=$5,set_by=NULLIF($6,''),updated_at=$7
WHERE org_id=$1 AND container=$2
RETURNING container,orders,bots,ambient_enabled,set_by,updated_at`, r.orgID, container, orders, string(encodedBots), ambient, setBy, at))
	}
	if err != nil {
		return nil, false, err
	}
	encodedBots, err = json.Marshal(policy.Bots)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(ctx, "INSERT INTO channel_policy_history(org_id,container,orders,bots,ambient_enabled,set_by,created_at) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7)", r.orgID, policy.Container, policy.Orders, string(encodedBots), policy.AmbientEnabled, policy.SetBy, at)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return policy, false, nil
}

type channelPolicyScanner interface{ Scan(...any) error }

func scanChannelPolicy(row channelPolicyScanner) (*ChannelPolicy, error) {
	var policy ChannelPolicy
	var bots []byte
	if err := row.Scan(&policy.Container, &policy.Orders, &bots, &policy.AmbientEnabled, &policy.SetBy, &policy.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(bots, &policy.Bots); err != nil {
		return nil, err
	}
	if policy.Bots == nil {
		policy.Bots = map[string]any{}
	}
	return &policy, nil
}
