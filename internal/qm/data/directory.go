package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type DirectoryMember struct {
	PrincipalID string `json:"principalId"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
	SlackID     string `json:"slackId,omitempty"`
}

type DirectoryChannel struct {
	ChannelID string `json:"channelId"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"isPrivate,omitempty"`
}

type DirectoryPair struct {
	ChannelID   string `json:"channelId,omitempty"`
	GroupID     string `json:"groupId,omitempty"`
	PrincipalID string `json:"principalId"`
}

type DirectoryUpdate struct {
	Members          *[]DirectoryMember
	Channels         *[]DirectoryChannel
	ChannelMembers   *[]DirectoryPair
	GroupMembers     *[]DirectoryPair
	MembersSyncedAt  *int64
	ChannelsSyncedAt *int64
	GroupsSyncedAt   *int64
}

type DirectoryRepository struct {
	pg    *Postgres
	orgID string
}

func NewDirectoryRepository(pg *Postgres, orgID string) *DirectoryRepository {
	return &DirectoryRepository{pg: pg, orgID: orgID}
}

func (r *DirectoryRepository) Sync(ctx context.Context, update DirectoryUpdate) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('directory'), hashtext($1))", r.orgID); err != nil {
		return err
	}
	if update.Members != nil {
		members := canonicalMembers(*update.Members)
		rows := make([]string, 0, len(members))
		for _, m := range members {
			rows = append(rows, strings.Join([]string{m.PrincipalID, m.DisplayName, m.Type, m.SlackID}, "|"))
		}
		if ok, err := r.applySnapshot(ctx, tx, "members", snapshotHash(rows), update.MembersSyncedAt); err != nil {
			return err
		} else if ok {
			if _, err = tx.Exec(ctx, "DELETE FROM directory_members WHERE org_id=$1", r.orgID); err != nil {
				return err
			}
			for _, member := range members {
				if _, err = tx.Exec(ctx, `INSERT INTO directory_members(org_id,principal_id,display_name,display_name_lc,type,slack_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,''))`, r.orgID, member.PrincipalID, member.DisplayName, normalizeDirectory(member.DisplayName), member.Type, member.SlackID); err != nil {
					return err
				}
			}
		}
	}
	if update.Channels != nil {
		channels := canonicalChannels(*update.Channels)
		members := []DirectoryPair(nil)
		knownMembers := update.ChannelMembers != nil
		if knownMembers {
			members = canonicalChannelMembers(*update.ChannelMembers)
		}
		rows := make([]string, 0, len(channels)+len(members)+1)
		for _, c := range channels {
			rows = append(rows, c.ChannelID+"|"+c.Name+"|"+boolText(c.IsPrivate))
		}
		if knownMembers {
			rows = append(rows, "members:known")
			for _, m := range members {
				rows = append(rows, m.ChannelID+"|"+m.PrincipalID)
			}
		}
		if ok, err := r.applySnapshot(ctx, tx, "channels", snapshotHash(rows), update.ChannelsSyncedAt); err != nil {
			return err
		} else if ok {
			if _, err = tx.Exec(ctx, "DELETE FROM directory_channels WHERE org_id=$1", r.orgID); err != nil {
				return err
			}
			for _, channel := range channels {
				if _, err = tx.Exec(ctx, `INSERT INTO directory_channels(org_id,channel_id,name,name_lc,is_private) VALUES($1,$2,$3,$4,$5)`, r.orgID, channel.ChannelID, channel.Name, normalizeDirectory(channel.Name), channel.IsPrivate); err != nil {
					return err
				}
			}
			if knownMembers {
				if _, err = tx.Exec(ctx, "DELETE FROM directory_channel_members WHERE org_id=$1", r.orgID); err != nil {
					return err
				}
				for _, member := range members {
					if _, err = tx.Exec(ctx, "INSERT INTO directory_channel_members(org_id,channel_id,principal_id) VALUES($1,$2,$3)", r.orgID, member.ChannelID, member.PrincipalID); err != nil {
						return err
					}
				}
				if _, err = tx.Exec(ctx, "UPDATE directory_sync SET channel_members_synced=TRUE WHERE org_id=$1", r.orgID); err != nil {
					return err
				}
			}
		}
	}
	if update.GroupMembers != nil {
		members := canonicalGroupMembers(*update.GroupMembers)
		rows := make([]string, 0, len(members))
		for _, m := range members {
			rows = append(rows, m.GroupID+"|"+m.PrincipalID)
		}
		if ok, err := r.applySnapshot(ctx, tx, "groups", snapshotHash(rows), update.GroupsSyncedAt); err != nil {
			return err
		} else if ok {
			if _, err = tx.Exec(ctx, "DELETE FROM directory_group_members WHERE org_id=$1", r.orgID); err != nil {
				return err
			}
			for _, member := range members {
				if _, err = tx.Exec(ctx, "INSERT INTO directory_group_members(org_id,group_id,principal_id) VALUES($1,$2,$3)", r.orgID, member.GroupID, member.PrincipalID); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit(ctx)
}

func (r *DirectoryRepository) applySnapshot(ctx context.Context, tx pgx.Tx, kind, hash string, syncedAt *int64) (bool, error) {
	hashColumn := kind + "_hash"
	timeColumn := kind + "_synced_at"
	var currentHash *string
	var currentTime *int64
	query := "SELECT " + hashColumn + ", " + timeColumn + " FROM directory_sync WHERE org_id=$1"
	err := tx.QueryRow(ctx, query, r.orgID).Scan(&currentHash, &currentTime)
	if err != nil && err != pgx.ErrNoRows {
		return false, err
	}
	if syncedAt != nil && currentTime != nil && *currentTime > *syncedAt {
		return false, nil
	}
	if currentHash != nil && *currentHash == hash {
		return false, nil
	}
	now := time.Now().UnixMilli()
	_, err = tx.Exec(ctx, "INSERT INTO directory_sync(org_id,"+hashColumn+","+timeColumn+",updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(org_id) DO UPDATE SET "+hashColumn+"=EXCLUDED."+hashColumn+", "+timeColumn+"=COALESCE(EXCLUDED."+timeColumn+",directory_sync."+timeColumn+"), updated_at=EXCLUDED.updated_at", r.orgID, hash, syncedAt, now)
	return err == nil, err
}

func (r *DirectoryRepository) SetWorkspaceURL(ctx context.Context, value string) error {
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO directory_meta(org_id,workspace_url,updated_at) VALUES($1,$2,$3)
ON CONFLICT(org_id) DO UPDATE SET workspace_url=EXCLUDED.workspace_url,updated_at=EXCLUDED.updated_at`, r.orgID, value, time.Now().UnixMilli())
	return err
}

func (r *DirectoryRepository) Meta(ctx context.Context) (map[string]any, error) {
	var value *string
	err := r.pg.Pool.QueryRow(ctx, "SELECT workspace_url FROM directory_meta WHERE org_id=$1", r.orgID).Scan(&value)
	if err == pgx.ErrNoRows {
		return map[string]any{"workspaceUrl": nil}, nil
	}
	return map[string]any{"workspaceUrl": value}, err
}

func (r *DirectoryRepository) Resolve(ctx context.Context, query string) ([]DirectoryMember, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT principal_id,display_name,type,slack_id FROM directory_members
WHERE org_id=$1 AND (slack_id=$2 OR lower(principal_id)=lower($2) OR display_name_lc=$3 OR display_name_lc LIKE $4)
ORDER BY CASE WHEN slack_id=$2 OR lower(principal_id)=lower($2) OR display_name_lc=$3 THEN 0 ELSE 1 END,display_name_lc,principal_id LIMIT 11`, r.orgID, query, normalizeDirectory(query), "%"+escapeLike(normalizeDirectory(query))+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []DirectoryMember{}
	for rows.Next() {
		var member DirectoryMember
		var slackID *string
		if err := rows.Scan(&member.PrincipalID, &member.DisplayName, &member.Type, &slackID); err != nil {
			return nil, err
		}
		if slackID != nil {
			member.SlackID = *slackID
		}
		results = append(results, member)
	}
	return results, rows.Err()
}

func (r *DirectoryRepository) List(ctx context.Context) ([]DirectoryMember, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT principal_id,display_name,type,slack_id FROM directory_members WHERE org_id=$1 ORDER BY display_name_lc,principal_id", r.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DirectoryMember{}
	for rows.Next() {
		var member DirectoryMember
		var slackID *string
		if err := rows.Scan(&member.PrincipalID, &member.DisplayName, &member.Type, &slackID); err != nil {
			return nil, err
		}
		if slackID != nil {
			member.SlackID = *slackID
		}
		result = append(result, member)
	}
	return result, rows.Err()
}

// Get resolves the canonical directory record for a principal. Email-like IDs
// retain the Node directory store's case-insensitive fallback; opaque IDs are
// exact matches.
func (r *DirectoryRepository) Get(ctx context.Context, principalID string) (*DirectoryMember, error) {
	key := strings.TrimSpace(principalID)
	if key == "" {
		return nil, nil
	}
	var member DirectoryMember
	var slackID *string
	err := r.pg.Pool.QueryRow(ctx, `SELECT principal_id,display_name,type,slack_id FROM directory_members
WHERE org_id=$1 AND (principal_id=$2 OR ($3 AND lower(principal_id)=lower($2)))
ORDER BY (principal_id=$2) DESC LIMIT 1`, r.orgID, key, strings.Contains(key, "@")).Scan(&member.PrincipalID, &member.DisplayName, &member.Type, &slackID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if slackID != nil {
		member.SlackID = *slackID
	}
	return &member, nil
}

func (r *DirectoryRepository) SetActive(ctx context.Context, principalID string, active bool) error {
	key := strings.ToLower(principalID)
	if active {
		_, err := r.pg.Pool.Exec(ctx, "DELETE FROM deactivated_principals WHERE id=$1", key)
		return err
	}
	payload, err := json.Marshal(map[string]any{"principalId": principalID, "source": "manual", "at": time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	_, err = r.pg.Pool.Exec(ctx, "INSERT INTO deactivated_principals(id,json) VALUES($1,$2::jsonb) ON CONFLICT(id) DO UPDATE SET json=EXCLUDED.json", key, payload)
	return err
}

func (r *DirectoryRepository) IsScopeMember(ctx context.Context, kind, container, principalID string) (bool, error) {
	var exists bool
	switch kind {
	case "channel":
		err := r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM directory_channel_members WHERE org_id=$1 AND channel_id=$2 AND principal_id=$3)", r.orgID, container, principalID).Scan(&exists)
		return exists, err
	case "group":
		err := r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM directory_group_members WHERE org_id=$1 AND group_id=$2 AND principal_id=$3)", r.orgID, container, principalID).Scan(&exists)
		return exists, err
	default:
		return false, nil
	}
}

func (r *DirectoryRepository) IsInternal(ctx context.Context, principalID string) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM directory_members WHERE org_id=$1 AND principal_id=$2 AND type='internal')", r.orgID, principalID).Scan(&exists)
	return exists, err
}

func (r *DirectoryRepository) Channel(ctx context.Context, channelID string) (*DirectoryChannel, error) {
	var channel DirectoryChannel
	err := r.pg.Pool.QueryRow(ctx, "SELECT channel_id,name,is_private FROM directory_channels WHERE org_id=$1 AND channel_id=$2", r.orgID, channelID).Scan(&channel.ChannelID, &channel.Name, &channel.IsPrivate)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &channel, nil
}

func (r *DirectoryRepository) ChannelsFor(ctx context.Context, principalID string) ([]DirectoryChannel, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT c.channel_id,c.name,c.is_private FROM directory_channels c
		JOIN directory_channel_members m ON m.org_id=c.org_id AND m.channel_id=c.channel_id
		WHERE c.org_id=$1 AND m.principal_id=$2 ORDER BY c.name_lc,c.channel_id`, r.orgID, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DirectoryChannel{}
	for rows.Next() {
		var item DirectoryChannel
		if err := rows.Scan(&item.ChannelID, &item.Name, &item.IsPrivate); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListChannels supports administrative scope discovery without exposing
// membership rows.
func (r *DirectoryRepository) ListChannels(ctx context.Context) ([]DirectoryChannel, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT channel_id,name,is_private FROM directory_channels WHERE org_id=$1 ORDER BY name_lc,channel_id", r.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DirectoryChannel{}
	for rows.Next() {
		var item DirectoryChannel
		if err := rows.Scan(&item.ChannelID, &item.Name, &item.IsPrivate); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *DirectoryRepository) GroupIDsFor(ctx context.Context, principalID string) ([]string, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT group_id FROM directory_group_members WHERE org_id=$1 AND principal_id=$2 ORDER BY group_id", r.orgID, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func normalizeDirectory(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
func snapshotHash(rows []string) string {
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:])
}
func boolText(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func escapeLike(value string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(value)
}
func canonicalMembers(values []DirectoryMember) []DirectoryMember {
	byID := map[string]DirectoryMember{}
	for _, value := range values {
		if value.PrincipalID != "" && value.DisplayName != "" && value.Type == "internal" {
			byID[value.PrincipalID] = value
		}
	}
	result := make([]DirectoryMember, 0, len(byID))
	for _, value := range byID {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PrincipalID < result[j].PrincipalID })
	return result
}
func canonicalChannels(values []DirectoryChannel) []DirectoryChannel {
	byID := map[string]DirectoryChannel{}
	for _, value := range values {
		if value.ChannelID != "" && value.Name != "" {
			byID[value.ChannelID] = value
		}
	}
	result := make([]DirectoryChannel, 0, len(byID))
	for _, value := range byID {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ChannelID < result[j].ChannelID })
	return result
}
func canonicalChannelMembers(values []DirectoryPair) []DirectoryPair {
	byKey := map[string]DirectoryPair{}
	for _, value := range values {
		if value.ChannelID != "" && value.PrincipalID != "" {
			byKey[value.ChannelID+"\x00"+value.PrincipalID] = value
		}
	}
	result := make([]DirectoryPair, 0, len(byKey))
	for _, value := range byKey {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ChannelID+result[i].PrincipalID < result[j].ChannelID+result[j].PrincipalID
	})
	return result
}
func canonicalGroupMembers(values []DirectoryPair) []DirectoryPair {
	byKey := map[string]DirectoryPair{}
	for _, value := range values {
		if value.GroupID != "" && value.PrincipalID != "" {
			byKey[value.GroupID+"\x00"+value.PrincipalID] = value
		}
	}
	result := make([]DirectoryPair, 0, len(byKey))
	for _, value := range byKey {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].GroupID+result[i].PrincipalID < result[j].GroupID+result[j].PrincipalID
	})
	return result
}
