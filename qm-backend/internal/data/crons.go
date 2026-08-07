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

type CronRecord struct {
	ID          string
	JSON        json.RawMessage
	Enabled     bool
	Archived    bool
	NextFireAt  *int64
	LastFiredAt *int64
	CreatedAt   int64
}

type CronRepository struct{ pg *Postgres }

func NewCronRepository(pg *Postgres) *CronRepository { return &CronRepository{pg: pg} }

func (r *CronRepository) Create(ctx context.Context, payload json.RawMessage, nextFireAt *int64) (CronRecord, error) {
	if !json.Valid(payload) {
		return CronRecord{}, errors.New("cron JSON is required")
	}
	now := time.Now().UnixMilli()
	record := CronRecord{ID: uuid.NewString(), JSON: payload, Enabled: true, CreatedAt: now, NextFireAt: nextFireAt}
	_, err := r.pg.Pool.Exec(ctx, "INSERT INTO crons(id,json,enabled,archived,next_fire_at,created_at,updated_at) VALUES($1,$2::jsonb,$3,$4,$5,$6,$6)", record.ID, string(payload), record.Enabled, record.Archived, nextFireAt, now)
	return record, err
}

func (r *CronRepository) Get(ctx context.Context, id string) (*CronRecord, error) {
	var record CronRecord
	err := r.pg.Pool.QueryRow(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons WHERE id=$1", id).Scan(&record.ID, &record.JSON, &record.Enabled, &record.Archived, &record.NextFireAt, &record.LastFiredAt, &record.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &record, err
}

func (r *CronRepository) List(ctx context.Context) ([]CronRecord, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectCrons(rows)
}

func (r *CronRepository) Put(ctx context.Context, id string, payload json.RawMessage) (*CronRecord, error) {
	if id == "" {
		return nil, errors.New("cron id is required")
	}
	fields, err := cronFields(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err = r.pg.Pool.Exec(ctx, `INSERT INTO crons(id,json,enabled,archived,next_fire_at,last_fired_at,created_at,updated_at)
VALUES($1,$2::jsonb,$3,$4,$5,$6,$7,$8)
ON CONFLICT(id) DO UPDATE SET json=EXCLUDED.json,enabled=EXCLUDED.enabled,archived=EXCLUDED.archived,next_fire_at=EXCLUDED.next_fire_at,last_fired_at=EXCLUDED.last_fired_at,updated_at=EXCLUDED.updated_at`, id, string(payload), fields.enabled, fields.archived, fields.nextFireAt, fields.lastFiredAt, fields.createdAt(now), now)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *CronRepository) PutIfAbsent(ctx context.Context, id string, payload json.RawMessage) (*CronRecord, error) {
	if id == "" {
		return nil, errors.New("cron id is required")
	}
	fields, err := cronFields(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	row := r.pg.Pool.QueryRow(ctx, `INSERT INTO crons(id,json,enabled,archived,next_fire_at,last_fired_at,created_at,updated_at)
VALUES($1,$2::jsonb,$3,$4,$5,$6,$7,$8)
ON CONFLICT(id) DO UPDATE SET json=crons.json
RETURNING id,json,enabled,archived,next_fire_at,last_fired_at,created_at`, id, string(payload), fields.enabled, fields.archived, fields.nextFireAt, fields.lastFiredAt, fields.createdAt(now), now)
	return scanCron(row)
}

func (r *CronRepository) Merge(ctx context.Context, id string, patch json.RawMessage, removeKeys []string) (*CronRecord, error) {
	if !json.Valid(patch) {
		return nil, errors.New("cron patch must be JSON")
	}
	var changes map[string]json.RawMessage
	if err := json.Unmarshal(patch, &changes); err != nil || changes == nil {
		return nil, errors.New("cron patch must be an object")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCron(tx.QueryRow(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(current.JSON, &document); err != nil {
		return nil, err
	}
	for _, key := range removeKeys {
		delete(document, key)
	}
	for key, value := range changes {
		document[key] = value
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	fields, err := cronFields(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	updated, err := scanCron(tx.QueryRow(ctx, `UPDATE crons SET json=$2::jsonb,enabled=$3,archived=$4,next_fire_at=$5,last_fired_at=$6,updated_at=$7 WHERE id=$1
RETURNING id,json,enabled,archived,next_fire_at,last_fired_at,created_at`, id, string(payload), fields.enabled, fields.archived, fields.nextFireAt, fields.lastFiredAt, now))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *CronRepository) Delete(ctx context.Context, id string) error {
	_, err := r.pg.Pool.Exec(ctx, "DELETE FROM crons WHERE id=$1", id)
	return err
}

func (r *CronRepository) Take(ctx context.Context, id string) (*CronRecord, error) {
	record, err := scanCron(r.pg.Pool.QueryRow(ctx, "DELETE FROM crons WHERE id=$1 RETURNING id,json,enabled,archived,next_fire_at,last_fired_at,created_at", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return record, err
}

func (r *CronRepository) ListDue(ctx context.Context, now int64, limit int) ([]CronRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons WHERE enabled AND NOT archived AND next_fire_at IS NOT NULL AND next_fire_at <= $1 ORDER BY next_fire_at LIMIT $2", now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectCrons(rows)
}

func (r *CronRepository) ClaimSlot(ctx context.Context, id string, scheduledAt, firedAt int64, nextFireAt *int64) (bool, error) {
	return r.updateSchedule(ctx, id, scheduledAt, pointer(firedAt), nextFireAt, true)
}

func (r *CronRepository) UnclaimSlot(ctx context.Context, id string, scheduledAt, firedAt int64, priorLastFiredAt *int64) (bool, error) {
	return r.updateSchedule(ctx, id, firedAt, priorLastFiredAt, &scheduledAt, false)
}

func (r *CronRepository) SetEnabled(ctx context.Context, id string, enabled bool) (bool, error) {
	patch, _ := json.Marshal(map[string]bool{"enabled": enabled, "archived": false})
	if !enabled {
		patch, _ = json.Marshal(map[string]bool{"enabled": false})
	}
	updated, err := r.Merge(ctx, id, patch, nil)
	return updated != nil, err
}

func (r *CronRepository) MarkFired(ctx context.Context, id string, firedAt int64, nextFireAt *int64) (bool, error) {
	return r.updateSchedule(ctx, id, -1, pointer(firedAt), nextFireAt, false)
}

func (r *CronRepository) RecordFire(ctx context.Context, id string, entry json.RawMessage) (bool, error) {
	var newEntry map[string]json.RawMessage
	if !json.Valid(entry) || json.Unmarshal(entry, &newEntry) != nil || len(newEntry["fireKey"]) == 0 {
		return false, errors.New("cron fire entry with fireKey is required")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCron(tx.QueryRow(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(current.JSON, &document); err != nil {
		return false, err
	}
	var log []map[string]json.RawMessage
	_ = json.Unmarshal(document["fireLog"], &log)
	key := string(newEntry["fireKey"])
	replaced := false
	for i, old := range log {
		if string(old["fireKey"]) == key {
			for field, value := range newEntry {
				old[field] = value
			}
			log[i] = old
			replaced = true
			break
		}
	}
	if !replaced {
		log = append(log, newEntry)
	}
	sort.SliceStable(log, func(i, j int) bool { return jsonNumber(log[i]["firedAt"]) < jsonNumber(log[j]["firedAt"]) })
	document["fireLog"], err = json.Marshal(log)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, "UPDATE crons SET json=$2::jsonb,updated_at=$3 WHERE id=$1", id, string(payload), time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *CronRepository) updateSchedule(ctx context.Context, id string, expectedLastOrNext int64, newLast, newNext *int64, claim bool) (bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCron(tx.QueryRow(ctx, "SELECT id,json,enabled,archived,next_fire_at,last_fired_at,created_at FROM crons WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if claim && (!current.Enabled || current.Archived || current.NextFireAt == nil || *current.NextFireAt != expectedLastOrNext) {
		return false, nil
	}
	if !claim && expectedLastOrNext >= 0 && (current.LastFiredAt == nil || *current.LastFiredAt != expectedLastOrNext) {
		return false, nil
	}
	payload, err := schedulePayload(current.JSON, newLast, newNext)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, "UPDATE crons SET json=$2::jsonb,last_fired_at=$3,next_fire_at=$4,updated_at=$5 WHERE id=$1", id, string(payload), newLast, newNext, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type cronFieldValues struct {
	enabled     bool
	archived    bool
	nextFireAt  *int64
	lastFiredAt *int64
	created     *int64
}

func (f cronFieldValues) createdAt(fallback int64) int64 {
	if f.created == nil {
		return fallback
	}
	return *f.created
}

func cronFields(payload json.RawMessage) (cronFieldValues, error) {
	if !json.Valid(payload) {
		return cronFieldValues{}, errors.New("cron JSON is required")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil || document == nil {
		return cronFieldValues{}, errors.New("cron JSON must be an object")
	}
	values := cronFieldValues{enabled: true}
	if raw := document["enabled"]; raw != nil {
		if err := json.Unmarshal(raw, &values.enabled); err != nil {
			return cronFieldValues{}, errors.New("cron enabled must be boolean")
		}
	}
	if raw := document["archived"]; raw != nil {
		if err := json.Unmarshal(raw, &values.archived); err != nil {
			return cronFieldValues{}, errors.New("cron archived must be boolean")
		}
	}
	var err error
	if values.nextFireAt, err = optionalJSONInt(document["nextFireAt"]); err != nil {
		return cronFieldValues{}, errors.New("cron nextFireAt must be an integer")
	}
	if values.lastFiredAt, err = optionalJSONInt(document["lastFiredAt"]); err != nil {
		return cronFieldValues{}, errors.New("cron lastFiredAt must be an integer")
	}
	if values.created, err = optionalJSONInt(document["createdAt"]); err != nil {
		return cronFieldValues{}, errors.New("cron createdAt must be an integer")
	}
	return values, nil
}

func schedulePayload(payload json.RawMessage, last, next *int64) (json.RawMessage, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	if last == nil {
		delete(document, "lastFiredAt")
	} else {
		document["lastFiredAt"], _ = json.Marshal(*last)
	}
	if next == nil {
		delete(document, "nextFireAt")
	} else {
		document["nextFireAt"], _ = json.Marshal(*next)
	}
	return json.Marshal(document)
}

func optionalJSONInt(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func jsonNumber(raw json.RawMessage) int64 {
	value, _ := optionalJSONInt(raw)
	if value == nil {
		return 0
	}
	return *value
}

func pointer(value int64) *int64 { return &value }

type cronScanner interface{ Scan(...any) error }
type cronRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanCron(row cronScanner) (*CronRecord, error) {
	var record CronRecord
	if err := row.Scan(&record.ID, &record.JSON, &record.Enabled, &record.Archived, &record.NextFireAt, &record.LastFiredAt, &record.CreatedAt); err != nil {
		return nil, err
	}
	return &record, nil
}

func collectCrons(rows cronRows) ([]CronRecord, error) {
	var records []CronRecord
	for rows.Next() {
		record, err := scanCron(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}
