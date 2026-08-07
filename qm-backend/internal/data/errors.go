package data

import (
	"context"
	"strconv"
	"strings"
)

type ErrorEvent struct {
	TS         int64   `json:"ts"`
	Category   string  `json:"category"`
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	ScopeLabel string  `json:"scopeLabel"`
	SessionID  *string `json:"sessionId,omitempty"`
}

type ErrorEventQuery struct {
	ScopeID   string
	SessionID string
	Limit     int
}

type ErrorEventRepository struct{ pg *Postgres }

func NewErrorEventRepository(pg *Postgres) *ErrorEventRepository {
	return &ErrorEventRepository{pg: pg}
}

func (r *ErrorEventRepository) List(ctx context.Context, query ErrorEventQuery) ([]ErrorEvent, error) {
	where, args := errorEventWhere(query.ScopeID, query.SessionID)
	limit := query.Limit
	if limit < 1 || limit > 200 {
		limit = 200
	}
	args = append(args, limit)
	rows, err := r.pg.Pool.Query(ctx, `SELECT ts,category,code,message,scope_label,session_id FROM error_events`+where+` ORDER BY ts DESC,id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ErrorEvent{}
	for rows.Next() {
		var item ErrorEvent
		if err := rows.Scan(&item.TS, &item.Category, &item.Code, &item.Message, &item.ScopeLabel, &item.SessionID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *ErrorEventRepository) Count(ctx context.Context, query ErrorEventQuery) (int, error) {
	where, args := errorEventWhere(query.ScopeID, query.SessionID)
	var count int
	err := r.pg.Pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM error_events`+where, args...).Scan(&count)
	return count, err
}

func errorEventWhere(scopeID, sessionID string) (string, []any) {
	conditions, args := []string{}, []any{}
	if scopeID = strings.TrimSpace(scopeID); scopeID != "" {
		args = append(args, scopeID)
		conditions = append(conditions, "scope_label=$"+strconv.Itoa(len(args)))
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, sessionID)
		conditions = append(conditions, "session_id=$"+strconv.Itoa(len(args)))
	}
	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}
