package data

import (
	"context"
	"sort"
	"strings"
	"time"
)

type EgressRecord struct {
	TS             int64   `json:"ts"`
	Source         string  `json:"source"`
	Host           string  `json:"host"`
	ScopeLabel     string  `json:"scopeLabel"`
	Allowed        bool    `json:"allowed"`
	Status         string  `json:"status"`
	Slug           *string `json:"slug,omitempty"`
	UpstreamStatus *int    `json:"upstreamStatus,omitempty"`
	PrincipalID    *string `json:"principalId,omitempty"`
	Port           *int    `json:"port,omitempty"`
	Via            *string `json:"via,omitempty"`
	PeerIP         *string `json:"peerIp,omitempty"`
}

type EgressSummary struct {
	Records  []EgressRecord `json:"records"`
	Total    int            `json:"total"`
	Denied   int            `json:"denied"`
	Hosts    int            `json:"hosts"`
	BySource struct {
		Broker   int `json:"broker"`
		Firewall int `json:"firewall"`
	} `json:"bySource"`
}

// CredentialUsageRecord is the safe broker-use event shape exposed to a
// credential owner. It intentionally excludes any request payload or secret.
type CredentialUsageRecord struct {
	TS             int64  `json:"ts"`
	Slug           string `json:"slug"`
	Host           string `json:"host"`
	Status         string `json:"status"`
	UpstreamStatus *int   `json:"upstreamStatus,omitempty"`
	ScopeLabel     string `json:"scopeLabel"`
	PrincipalID    string `json:"principalId"`
}

type EgressRepository struct{ pg *Postgres }

func NewEgressRepository(pg *Postgres) *EgressRepository { return &EgressRepository{pg: pg} }

type EgressAuditInput struct {
	Host        string
	Allowed     bool
	Verdict     string
	ScopeLabel  string
	Via         *string
	PeerIP      *string
	PrincipalID *string
	Port        *int
}

func (r *EgressRepository) RecordAuditBatch(ctx context.Context, records []EgressAuditInput) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UnixMilli()
	for _, record := range records {
		if _, err := tx.Exec(ctx, `INSERT INTO egress_events(ts,source,host,allowed,scope_label,port,verdict,via,peer_ip,principal_id)
VALUES($1,'proxy',$2,$3,$4,$5,$6,$7,$8,$9)`, now, record.Host, record.Allowed, record.ScopeLabel, record.Port, record.Verdict, record.Via, record.PeerIP, record.PrincipalID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *EgressRepository) CredentialUsage(ctx context.Context, slug string, limit int) ([]CredentialUsageRecord, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT ts,slug,host,status,upstream_status,scope_label,principal_id
FROM credential_usage WHERE slug=$1 ORDER BY ts DESC,id DESC LIMIT $2`, slug, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CredentialUsageRecord{}
	for rows.Next() {
		var item CredentialUsageRecord
		if err := rows.Scan(&item.TS, &item.Slug, &item.Host, &item.Status, &item.UpstreamStatus, &item.ScopeLabel, &item.PrincipalID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *EgressRepository) Summary(ctx context.Context, scope string, orgWide bool) (EgressSummary, error) {
	filter, args := "", []any{}
	if !orgWide {
		filter, args = " WHERE scope_label=$1", []any{scope}
	}
	brokerRows, err := r.pg.Pool.Query(ctx, `SELECT ts,slug,host,status,upstream_status,scope_label,principal_id
FROM credential_usage`+filter+` ORDER BY ts DESC,id DESC LIMIT 1000`, args...)
	if err != nil {
		return EgressSummary{}, err
	}
	defer brokerRows.Close()
	result := EgressSummary{Records: []EgressRecord{}}
	for brokerRows.Next() {
		var item EgressRecord
		if err := brokerRows.Scan(&item.TS, &item.Slug, &item.Host, &item.Status, &item.UpstreamStatus, &item.ScopeLabel, &item.PrincipalID); err != nil {
			return result, err
		}
		item.Source = "broker"
		item.Allowed = item.Status != "denied"
		result.Records = append(result.Records, item)
		result.BySource.Broker++
	}
	if err := brokerRows.Err(); err != nil {
		return result, err
	}
	firewallRows, err := r.pg.Pool.Query(ctx, `SELECT ts,source,host,allowed,scope_label,port,verdict,via,peer_ip,principal_id
FROM egress_events`+filter+` ORDER BY ts DESC,id DESC LIMIT 1000`, args...)
	if err != nil {
		return result, err
	}
	defer firewallRows.Close()
	for firewallRows.Next() {
		var item EgressRecord
		var verdict *string
		if err := firewallRows.Scan(&item.TS, &item.Source, &item.Host, &item.Allowed, &item.ScopeLabel, &item.Port, &verdict, &item.Via, &item.PeerIP, &item.PrincipalID); err != nil {
			return result, err
		}
		if verdict != nil {
			item.Status = *verdict
		} else if item.Allowed {
			item.Status = "ok"
		} else {
			item.Status = "denied"
		}
		result.Records = append(result.Records, item)
		result.BySource.Firewall++
	}
	if err := firewallRows.Err(); err != nil {
		return result, err
	}
	sort.SliceStable(result.Records, func(i, j int) bool { return result.Records[i].TS > result.Records[j].TS })
	if len(result.Records) > 1000 {
		result.Records = result.Records[:1000]
	}
	result.Total = len(result.Records)
	hosts := map[string]bool{}
	for _, item := range result.Records {
		if !item.Allowed {
			result.Denied++
		}
		if host := strings.TrimSpace(item.Host); host != "" {
			hosts[host] = true
		}
	}
	result.Hosts = len(hosts)
	return result, nil
}
