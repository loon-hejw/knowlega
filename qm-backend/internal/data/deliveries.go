package data

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DeliveryRepository struct{ pg *Postgres }

type ShadowDelivery struct {
	ID             string
	Destination    json.RawMessage
	Text           string
	IdempotencyKey string
	CreatedAt      int64
	Provenance     json.RawMessage
}

type PendingDelivery struct {
	ID               string          `json:"id"`
	Destination      json.RawMessage `json:"destination"`
	Text             string          `json:"text"`
	Attachments      json.RawMessage `json:"attachments,omitempty"`
	Provenance       json.RawMessage `json:"provenance,omitempty"`
	IdempotencyKey   string          `json:"idempotencyKey"`
	CreatedAt        int64           `json:"createdAt"`
	DeliveredAt      *int64          `json:"deliveredAt"`
	Shadow           bool            `json:"shadow,omitempty"`
	RecipientThread  *string         `json:"recipientThreadRef,omitempty"`
	DeliverLatencyMS *int            `json:"deliverLatencyMs,omitempty"`
	SlackAPIMS       *int            `json:"slackApiMs,omitempty"`
}

func NewDeliveryRepository(pg *Postgres) *DeliveryRepository { return &DeliveryRepository{pg: pg} }

// DeliveryEnqueueInput is the durable handoff consumed by Node's existing
// delivery worker. It records intent only; it never performs an external send.
type DeliveryEnqueueInput struct {
	Destination    json.RawMessage
	Text           string
	IdempotencyKey string
	Shadow         bool
}

// Enqueue mirrors Node's PostgreSQL delivery store for callers that only need
// to add an idempotent delivery. The Node worker remains responsible for
// claiming the row and making the actual provider call.
func (r *DeliveryRepository) Enqueue(ctx context.Context, input DeliveryEnqueueInput) error {
	if !json.Valid(input.Destination) || input.IdempotencyKey == "" {
		return errors.New("delivery destination and idempotency key are required")
	}
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO deliveries(id,idempotency_key,destination,text,created_at,shadow)
VALUES($1,$2,$3::jsonb,$4,$5,$6)
ON CONFLICT(idempotency_key) DO NOTHING`, uuid.NewString(), input.IdempotencyKey, string(input.Destination), input.Text, time.Now().UnixMilli(), input.Shadow)
	return err
}

func (r *DeliveryRepository) AckByKey(ctx context.Context, idempotencyKey string) error {
	now := time.Now().UnixMilli()
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO deliveries(id,idempotency_key,destination,text,created_at,delivered_at)
VALUES($1,$2,'{"type":"ack-tombstone","target":""}'::jsonb,'',$3,$3)
ON CONFLICT(idempotency_key) DO UPDATE SET delivered_at=COALESCE(deliveries.delivered_at,EXCLUDED.delivered_at)`, uuid.NewString(), idempotencyKey, now)
	return err
}

func (r *DeliveryRepository) Ack(ctx context.Context, id, recipientThreadRef string, slackAPIMS *float64) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var destination json.RawMessage
	err = tx.QueryRow(ctx, "SELECT destination FROM deliveries WHERE id=$1 FOR UPDATE", id).Scan(&destination)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	var target struct {
		Type   string `json:"type"`
		Target string `json:"target"`
	}
	if recipientThreadRef != "" && json.Unmarshal(destination, &target) == nil && target.Type == "principal" {
		var sessionID string
		newID := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO sessions(id,type,scope_id,thread_ref,created_at,last_activity,messages,turns)
VALUES($1,'dm',$2,$3,$4,$4,0,0) ON CONFLICT(thread_ref) DO NOTHING`, newID, "personal:"+target.Target, recipientThreadRef, now); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT id FROM sessions WHERE thread_ref=$1", recipientThreadRef).Scan(&sessionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET recipient_thread_ref=$2,delivered_at=COALESCE(delivered_at,$3)
WHERE id=$1 AND destination->>'type'='principal'`, id, recipientThreadRef, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `WITH boundary AS (
  SELECT COALESCE(MAX(seq) + 1, 0) AS seq FROM session_entries WHERE session_id=$1
)
INSERT INTO participants(session_id,principal_id,valid_from,valid_to,valid_from_seq,valid_to_seq,title)
SELECT $1,$2,$3,NULL,boundary.seq,NULL,NULL FROM boundary
ON CONFLICT(session_id,principal_id) DO UPDATE
SET valid_from=CASE WHEN participants.valid_to IS NULL THEN participants.valid_from ELSE EXCLUDED.valid_from END,
    valid_to=NULL,
    valid_from_seq=CASE WHEN participants.valid_to IS NULL THEN participants.valid_from_seq ELSE EXCLUDED.valid_from_seq END,
    valid_to_seq=NULL
WHERE participants.valid_to IS NOT NULL`, sessionID, target.Target, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries
SET delivered_at=$2,deliver_latency_ms=GREATEST(0,$2-created_at),slack_api_ms=COALESCE($3,slack_api_ms)
WHERE id=$1 AND delivered_at IS NULL`, id, now, slackAPIMS); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DeliveryRepository) ListShadow(ctx context.Context, limit int) ([]ShadowDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,destination,text,idempotency_key,created_at,provenance
FROM deliveries WHERE shadow=TRUE AND delivered_at IS NULL ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ShadowDelivery{}
	for rows.Next() {
		var item ShadowDelivery
		if err := rows.Scan(&item.ID, &item.Destination, &item.Text, &item.IdempotencyKey, &item.CreatedAt, &item.Provenance); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *DeliveryRepository) SentCountsBySourceSessions(ctx context.Context, sessions []SessionSummary) (map[string]int, error) {
	result := map[string]int{}
	if len(sessions) == 0 {
		return result, nil
	}
	ids, threads := make([]string, 0, len(sessions)), make([]string, 0, len(sessions))
	byThread := map[string]string{}
	for _, session := range sessions {
		ids = append(ids, session.ID)
		threads = append(threads, session.ThreadRef)
		byThread[session.ThreadRef] = session.ID
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT provenance->>'sourceSessionId',provenance->>'sourceThreadRef',COUNT(*)::int FROM deliveries WHERE NOT shadow AND (provenance->>'sourceSessionId'=ANY($1) OR (provenance->>'sourceSessionId' IS NULL AND provenance->>'sourceThreadRef'=ANY($2))) GROUP BY 1,2`, ids, threads)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, thread *string
		var count int
		if err := rows.Scan(&id, &thread, &count); err != nil {
			return nil, err
		}
		key := ""
		if id != nil {
			key = *id
		} else if thread != nil {
			key = byThread[*thread]
		}
		if key != "" {
			result[key] += count
		}
	}
	return result, rows.Err()
}

func (r *DeliveryRepository) SentRunCountsByCron(ctx context.Context, cronIDs []string) (map[string]int, error) {
	result := map[string]int{}
	if len(cronIDs) == 0 {
		return result, nil
	}
	wanted := map[string]bool{}
	for _, id := range cronIDs {
		wanted[id] = true
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT COALESCE(provenance->>'sourceSessionId',provenance->>'sourceThreadRef'),provenance->>'sourceThreadRef' FROM deliveries WHERE NOT shadow AND provenance->>'sourceThreadRef' IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]map[string]bool{}
	for rows.Next() {
		var run, thread string
		if err := rows.Scan(&run, &thread); err != nil {
			return nil, err
		}
		id := sessionCronID(thread)
		if !wanted[id] {
			continue
		}
		if seen[id] == nil {
			seen[id] = map[string]bool{}
		}
		seen[id][run] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, runs := range seen {
		result[id] = len(runs)
	}
	return result, nil
}

func (r *DeliveryRepository) ListByRecipientThread(ctx context.Context, threadRef string, limit int) ([]PendingDelivery, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,destination,text,attachments,provenance,idempotency_key,created_at,delivered_at,shadow,recipient_thread_ref,deliver_latency_ms,slack_api_ms FROM deliveries WHERE recipient_thread_ref=$1 AND destination->>'type'='principal' ORDER BY created_at DESC LIMIT $2`, threadRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result, err := scanPendingDeliveries(rows)
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt < result[j].CreatedAt })
	return result, nil
}

func (r *DeliveryRepository) ListBySourceSession(ctx context.Context, sessionID, threadRef string, limit int) ([]PendingDelivery, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,destination,text,attachments,provenance,idempotency_key,created_at,delivered_at,shadow,recipient_thread_ref,deliver_latency_ms,slack_api_ms FROM deliveries WHERE provenance->>'sourceSessionId'=$1 OR provenance->>'sourceThreadRef'=$2 ORDER BY created_at DESC LIMIT $3`, sessionID, threadRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result, err := scanPendingDeliveries(rows)
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt < result[j].CreatedAt })
	return result, nil
}

func (r *DeliveryRepository) Pending(ctx context.Context, kind string) ([]PendingDelivery, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,destination,text,attachments,provenance,idempotency_key,created_at,delivered_at,shadow,recipient_thread_ref,deliver_latency_ms,slack_api_ms
FROM deliveries WHERE delivered_at IS NULL AND NOT shadow AND destination->>'type'=$1`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPendingDeliveries(rows)
}

func (r *DeliveryRepository) ClaimPending(ctx context.Context, kind string, claimMillis float64) ([]PendingDelivery, error) {
	rows, err := r.pg.Pool.Query(ctx, `UPDATE deliveries
SET claim_expires_at=(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT + $2
WHERE id IN (
  SELECT id FROM deliveries
  WHERE delivered_at IS NULL AND NOT shadow AND destination->>'type'=$1
    AND (claim_expires_at IS NULL OR claim_expires_at <= (EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT)
  ORDER BY created_at FOR UPDATE SKIP LOCKED
)
RETURNING id,destination,text,attachments,provenance,idempotency_key,created_at,delivered_at,shadow,recipient_thread_ref,deliver_latency_ms,slack_api_ms`, kind, claimMillis)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result, err := scanPendingDeliveries(rows)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt < result[j].CreatedAt })
	return result, nil
}

type pendingDeliveryRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanPendingDeliveries(rows pendingDeliveryRows) ([]PendingDelivery, error) {
	result := []PendingDelivery{}
	for rows.Next() {
		var item PendingDelivery
		if err := rows.Scan(&item.ID, &item.Destination, &item.Text, &item.Attachments, &item.Provenance, &item.IdempotencyKey, &item.CreatedAt, &item.DeliveredAt, &item.Shadow, &item.RecipientThread, &item.DeliverLatencyMS, &item.SlackAPIMS); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
