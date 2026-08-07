package data

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type AmbientJudgment struct {
	ID        int64   `json:"id"`
	OrgID     string  `json:"orgId"`
	Surface   string  `json:"surface"`
	Container string  `json:"container"`
	Decision  string  `json:"decision"`
	Reason    *string `json:"reason,omitempty"`
	AskedBy   *string `json:"askedBy,omitempty"`
	Prompt    *string `json:"prompt,omitempty"`
	Model     *string `json:"model,omitempty"`
	LatencyMS *int    `json:"latencyMs,omitempty"`
	TSFrom    *string `json:"tsFrom,omitempty"`
	TSTo      *string `json:"tsTo,omitempty"`
	CreatedAt int64   `json:"createdAt"`
}

type AmbientJudgmentCounts struct {
	Act      int `json:"act"`
	Ignore   int `json:"ignore"`
	Fastlane int `json:"fastlane"`
}

type AmbientJudgmentQuery struct {
	Container string
	Decisions []string
	Before    *int64
	BeforeID  *int64
	Limit     int
}

type AmbientJudgmentRepository struct {
	pg    *Postgres
	orgID string
}

func NewAmbientJudgmentRepository(pg *Postgres, orgID string) *AmbientJudgmentRepository {
	return &AmbientJudgmentRepository{pg: pg, orgID: orgID}
}

func (r *AmbientJudgmentRepository) List(ctx context.Context, query AmbientJudgmentQuery) ([]AmbientJudgment, error) {
	conditions, args := observationConditions(r.orgID, "container", query.Container, "decision", query.Decisions, query.Before, query.BeforeID)
	args = append(args, observationLimit(query.Limit))
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,org_id,surface,container,decision,reason,asked_by,model,latency_ms,ts_from,ts_to,created_at
FROM ambient_judgments WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at DESC,id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []AmbientJudgment{}
	for rows.Next() {
		var item AmbientJudgment
		if err := rows.Scan(&item.ID, &item.OrgID, &item.Surface, &item.Container, &item.Decision, &item.Reason, &item.AskedBy, &item.Model, &item.LatencyMS, &item.TSFrom, &item.TSTo, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *AmbientJudgmentRepository) Get(ctx context.Context, id int64) (*AmbientJudgment, error) {
	var item AmbientJudgment
	err := r.pg.Pool.QueryRow(ctx, `SELECT id,org_id,surface,container,decision,reason,asked_by,prompt,model,latency_ms,ts_from,ts_to,created_at
FROM ambient_judgments WHERE org_id=$1 AND id=$2`, r.orgID, id).Scan(&item.ID, &item.OrgID, &item.Surface, &item.Container, &item.Decision, &item.Reason, &item.AskedBy, &item.Prompt, &item.Model, &item.LatencyMS, &item.TSFrom, &item.TSTo, &item.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *AmbientJudgmentRepository) Counts(ctx context.Context, container string) (AmbientJudgmentCounts, error) {
	result := AmbientJudgmentCounts{}
	conditions, args := observationConditions(r.orgID, "container", container, "", nil, nil, nil)
	rows, err := r.pg.Pool.Query(ctx, `SELECT decision,COUNT(*)::int FROM ambient_judgments WHERE `+strings.Join(conditions, " AND ")+` GROUP BY decision`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var decision string
		var count int
		if err := rows.Scan(&decision, &count); err != nil {
			return result, err
		}
		switch decision {
		case "act":
			result.Act = count
		case "ignore":
			result.Ignore = count
		case "fastlane":
			result.Fastlane = count
		}
	}
	return result, rows.Err()
}

type AckEmojiPick struct {
	ID         int64   `json:"id"`
	OrgID      string  `json:"orgId"`
	Surface    string  `json:"surface"`
	Channel    string  `json:"channel"`
	TS         string  `json:"ts"`
	Outcome    string  `json:"outcome"`
	Picked     *string `json:"picked,omitempty"`
	Icon       *string `json:"icon,omitempty"`
	Message    *string `json:"message,omitempty"`
	Candidates *string `json:"candidates,omitempty"`
	Model      *string `json:"model,omitempty"`
	LatencyMS  *int    `json:"latencyMs,omitempty"`
	CreatedAt  int64   `json:"createdAt"`
}

type AckEmojiPickCounts struct {
	Picked   int `json:"picked"`
	Declined int `json:"declined"`
}

type AckEmojiPickQuery struct {
	Channel  string
	Outcomes []string
	Before   *int64
	BeforeID *int64
	Limit    int
}

type AckEmojiPickRepository struct {
	pg    *Postgres
	orgID string
}

func NewAckEmojiPickRepository(pg *Postgres, orgID string) *AckEmojiPickRepository {
	return &AckEmojiPickRepository{pg: pg, orgID: orgID}
}

func (r *AckEmojiPickRepository) List(ctx context.Context, query AckEmojiPickQuery) ([]AckEmojiPick, error) {
	conditions, args := observationConditions(r.orgID, "channel", query.Channel, "outcome", query.Outcomes, query.Before, query.BeforeID)
	args = append(args, observationLimit(query.Limit))
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,org_id,surface,channel,ts,outcome,picked,icon,message,model,latency_ms,created_at
FROM ack_emoji_picks WHERE `+strings.Join(conditions, " AND ")+` ORDER BY created_at DESC,id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []AckEmojiPick{}
	for rows.Next() {
		var item AckEmojiPick
		if err := rows.Scan(&item.ID, &item.OrgID, &item.Surface, &item.Channel, &item.TS, &item.Outcome, &item.Picked, &item.Icon, &item.Message, &item.Model, &item.LatencyMS, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *AckEmojiPickRepository) Get(ctx context.Context, id int64) (*AckEmojiPick, error) {
	var item AckEmojiPick
	err := r.pg.Pool.QueryRow(ctx, `SELECT id,org_id,surface,channel,ts,outcome,picked,icon,message,candidates,model,latency_ms,created_at
FROM ack_emoji_picks WHERE org_id=$1 AND id=$2`, r.orgID, id).Scan(&item.ID, &item.OrgID, &item.Surface, &item.Channel, &item.TS, &item.Outcome, &item.Picked, &item.Icon, &item.Message, &item.Candidates, &item.Model, &item.LatencyMS, &item.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *AckEmojiPickRepository) Counts(ctx context.Context, channel string) (AckEmojiPickCounts, error) {
	result := AckEmojiPickCounts{}
	conditions, args := observationConditions(r.orgID, "channel", channel, "", nil, nil, nil)
	rows, err := r.pg.Pool.Query(ctx, `SELECT outcome,COUNT(*)::int FROM ack_emoji_picks WHERE `+strings.Join(conditions, " AND ")+` GROUP BY outcome`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			return result, err
		}
		switch outcome {
		case "picked":
			result.Picked = count
		case "declined":
			result.Declined = count
		}
	}
	return result, rows.Err()
}

func observationConditions(orgID, scopeColumn, scope string, kindColumn string, kinds []string, before, beforeID *int64) ([]string, []any) {
	conditions, args := []string{"org_id=$1"}, []any{orgID}
	if scope = strings.TrimSpace(scope); scope != "" {
		args = append(args, scope)
		conditions = append(conditions, scopeColumn+"=$"+strconv.Itoa(len(args)))
	}
	if kindColumn != "" && len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			args = append(args, kind)
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
		conditions = append(conditions, kindColumn+" IN ("+strings.Join(placeholders, ",")+")")
	}
	if before != nil {
		args = append(args, *before)
		placeholder := "$" + strconv.Itoa(len(args))
		if beforeID != nil {
			args = append(args, *beforeID)
			conditions = append(conditions, "(created_at < "+placeholder+" OR (created_at = "+placeholder+" AND id < $"+strconv.Itoa(len(args))+"))")
		} else {
			conditions = append(conditions, "created_at < "+placeholder)
		}
	}
	return conditions, args
}

func observationLimit(value int) int {
	if value < 1 {
		return 100
	}
	if value > 1000 {
		return 1000
	}
	return value
}
