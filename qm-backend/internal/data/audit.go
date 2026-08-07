package data

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type AuditEvent struct {
	PrincipalID    string
	Action         string
	Resource       string
	ScopeLabel     string
	Status         string
	Detail         string
	IdempotencyKey string
}

type Auditor struct{ pg *Postgres }

type AuditRecord struct {
	At          int64
	PrincipalID string
	Action      string
	Resource    string
	ScopeLabel  string
	Status      string
}

func NewAuditor(pg *Postgres) *Auditor { return &Auditor{pg: pg} }

func (a *Auditor) Record(ctx context.Context, event AuditEvent) error {
	_, err := a.pg.Pool.Exec(ctx, `INSERT INTO audit_log(at,principal_id,action,resource,scope_label,status,detail,idempotency_key)
VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''))
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, time.Now().UnixMilli(), event.PrincipalID, event.Action, event.Resource, event.ScopeLabel, event.Status, event.Detail, event.IdempotencyKey)
	return err
}

func (a *Auditor) Tail(ctx context.Context, scopeLabel string, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	query := "SELECT at,principal_id,action,resource,scope_label,status FROM audit_log"
	arguments := []any{}
	if strings.TrimSpace(scopeLabel) != "" {
		query += " WHERE scope_label=$1"
		arguments = append(arguments, scopeLabel)
	}
	query += " ORDER BY at DESC,id DESC LIMIT $" + strconv.Itoa(len(arguments)+1)
	arguments = append(arguments, limit)
	rows, err := a.pg.Pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AuditRecord{}
	for rows.Next() {
		var item AuditRecord
		var status *string
		if err := rows.Scan(&item.At, &item.PrincipalID, &item.Action, &item.Resource, &item.ScopeLabel, &status); err != nil {
			return nil, err
		}
		if status != nil {
			item.Status = *status
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
