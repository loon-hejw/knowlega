package data

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

type SurfaceContainer struct {
	Container    string   `json:"container"`
	LastTS       *string  `json:"lastTs,omitempty"`
	OldestTS     *string  `json:"oldestTs,omitempty"`
	Name         *string  `json:"name,omitempty"`
	Kind         *string  `json:"kind,omitempty"`
	Members      []string `json:"members"`
	MessageCount int      `json:"messageCount"`
	UpdatedAt    int64    `json:"updatedAt"`
}

type SurfaceMessage struct {
	Container    string            `json:"container"`
	TS           string            `json:"ts"`
	Sub          *string           `json:"sub,omitempty"`
	AuthorID     *string           `json:"authorId,omitempty"`
	AuthorName   *string           `json:"authorName,omitempty"`
	Text         string            `json:"text"`
	Mentions     map[string]string `json:"mentions,omitempty"`
	Self         bool              `json:"self,omitempty"`
	Bot          bool              `json:"bot,omitempty"`
	MentionsSelf bool              `json:"mentionsSelf,omitempty"`
	EditedAt     *int64            `json:"editedAt,omitempty"`
	Deleted      bool              `json:"deleted,omitempty"`
	Handled      bool              `json:"handled,omitempty"`
	CreatedAt    int64             `json:"createdAt"`
}

type SurfaceCacheRepository struct {
	pg    *Postgres
	orgID string
}

func NewSurfaceCacheRepository(pg *Postgres, orgID string) *SurfaceCacheRepository {
	return &SurfaceCacheRepository{pg: pg, orgID: orgID}
}

func (r *SurfaceCacheRepository) ListContainers(ctx context.Context, limit int) ([]SurfaceContainer, error) {
	limit = surfaceLimit(limit, 500)
	rows, err := r.pg.Pool.Query(ctx, `SELECT s.container,s.last_ts,s.oldest_ts,s.name,s.kind,s.members,COALESCE(m.n,0),s.updated_at
FROM channel_state s LEFT JOIN (
  SELECT container,COUNT(*)::int AS n FROM channel_messages WHERE org_id=$1 AND deleted=FALSE GROUP BY container
) m ON m.container=s.container WHERE s.org_id=$1 ORDER BY s.updated_at DESC LIMIT $2`, r.orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SurfaceContainer{}
	for rows.Next() {
		var item SurfaceContainer
		var members []byte
		if err := rows.Scan(&item.Container, &item.LastTS, &item.OldestTS, &item.Name, &item.Kind, &members, &item.MessageCount, &item.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(members, &item.Members)
		if item.Members == nil {
			item.Members = []string{}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SurfaceCacheRepository) Search(ctx context.Context, query, container string, limit int) ([]SurfaceMessage, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []SurfaceMessage{}, nil
	}
	limit = surfaceLimit(limit, 50)
	conditions, arguments := []string{"org_id=$1", "deleted=FALSE", "tsv @@ plainto_tsquery('english',$2)"}, []any{r.orgID, query}
	if container = strings.TrimSpace(container); container != "" {
		arguments = append(arguments, container)
		conditions = append(conditions, "container=$"+strconv.Itoa(len(arguments)))
	}
	arguments = append(arguments, limit)
	rows, err := r.pg.Pool.Query(ctx, `SELECT container,ts,sub,author_id,author_name,text,mentions,self,bot,mentions_self,edited_at,deleted,handled,created_at
FROM channel_messages WHERE `+strings.Join(conditions, " AND ")+` ORDER BY ts_rank(tsv,plainto_tsquery('english',$2)) DESC,ts DESC LIMIT $`+strconv.Itoa(len(arguments)), arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSurfaceMessages(rows)
}

func (r *SurfaceCacheRepository) Timeline(ctx context.Context, container, before string, limit int) ([]SurfaceMessage, error) {
	limit = surfaceLimit(limit, 200)
	arguments := []any{r.orgID, strings.TrimSpace(container)}
	condition := "org_id=$1 AND container=$2"
	if before = strings.TrimSpace(before); before != "" {
		arguments = append(arguments, before)
		condition += " AND ts < $" + strconv.Itoa(len(arguments))
	}
	arguments = append(arguments, limit)
	rows, err := r.pg.Pool.Query(ctx, `SELECT container,ts,sub,author_id,author_name,text,mentions,self,bot,mentions_self,edited_at,deleted,handled,created_at
FROM channel_messages WHERE `+condition+` ORDER BY ts DESC LIMIT $`+strconv.Itoa(len(arguments)), arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanSurfaceMessages(rows)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return items, nil
}

type surfaceRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanSurfaceMessages(rows surfaceRows) ([]SurfaceMessage, error) {
	result := []SurfaceMessage{}
	for rows.Next() {
		var item SurfaceMessage
		var mentions []byte
		if err := rows.Scan(&item.Container, &item.TS, &item.Sub, &item.AuthorID, &item.AuthorName, &item.Text, &mentions, &item.Self, &item.Bot, &item.MentionsSelf, &item.EditedAt, &item.Deleted, &item.Handled, &item.CreatedAt); err != nil {
			return nil, err
		}
		if len(mentions) > 0 {
			_ = json.Unmarshal(mentions, &item.Mentions)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func surfaceLimit(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	if value > 500 {
		return 500
	}
	return value
}
