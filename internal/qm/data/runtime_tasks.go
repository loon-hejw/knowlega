package data

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var runtimeTaskKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

type RuntimeTask struct {
	ID              string
	Kind            string
	PayloadVersion  int
	Payload         json.RawMessage
	ScopeID         *string
	ActorID         *string
	IdempotencyKey  *string
	SerialKey       *string
	Status          string
	Priority        int
	AvailableAt     int64
	Attempts        int
	MaxAttempts     int
	LeaseToken      *string
	LeaseOwner      *string
	LeaseExpiresAt  *int64
	CancelRequested bool
	Result          json.RawMessage
	LastError       *string
	CreatedAt       int64
	UpdatedAt       int64
	StartedAt       *int64
	FinishedAt      *int64
}

type EnqueueRuntimeTaskInput struct {
	ID             string
	Kind           string
	PayloadVersion int
	Payload        json.RawMessage
	ScopeID        string
	ActorID        string
	IdempotencyKey string
	SerialKey      string
	Priority       int
	AvailableAt    time.Time
	MaxAttempts    int
}

type ClaimedRuntimeTask struct {
	RuntimeTask
	ClaimedLeaseToken string
	ClaimedLeaseOwner string
}

type RuntimeTaskEvent struct {
	Sequence  int64
	Type      string
	Payload   json.RawMessage
	CreatedAt int64
}

type RuntimeTaskRepository struct{ pg *Postgres }

func NewRuntimeTaskRepository(pg *Postgres) *RuntimeTaskRepository {
	return &RuntimeTaskRepository{pg: pg}
}

func (r *RuntimeTaskRepository) Enqueue(ctx context.Context, input EnqueueRuntimeTaskInput) (string, bool, error) {
	input.Kind = strings.TrimSpace(input.Kind)
	if !runtimeTaskKindPattern.MatchString(input.Kind) {
		return "", false, errors.New("runtime task kind must be a lowercase slug")
	}
	if !json.Valid(input.Payload) {
		return "", false, errors.New("runtime task payload must be valid JSON")
	}
	if input.PayloadVersion <= 0 {
		input.PayloadVersion = 1
	}
	if input.MaxAttempts <= 0 {
		input.MaxAttempts = 3
	}
	now := time.Now()
	availableAt := input.AvailableAt
	if availableAt.IsZero() {
		availableAt = now
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.NewString()
	}
	row := r.pg.Pool.QueryRow(ctx, `INSERT INTO runtime_tasks(
id,kind,payload_version,payload,scope_id,actor_id,idempotency_key,serial_key,status,priority,available_at,max_attempts,created_at,updated_at)
VALUES($1,$2,$3,$4::jsonb,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),'pending',$9,$10,$11,$12,$12)
ON CONFLICT(kind,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING RETURNING id`,
		id, input.Kind, input.PayloadVersion, string(input.Payload), input.ScopeID, input.ActorID, input.IdempotencyKey,
		input.SerialKey, input.Priority, availableAt.UnixMilli(), input.MaxAttempts, now.UnixMilli())
	var inserted string
	if err := row.Scan(&inserted); err == nil {
		return inserted, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	if input.IdempotencyKey == "" {
		return "", false, errors.New("runtime task enqueue conflict without idempotency key")
	}
	if err := r.pg.Pool.QueryRow(ctx, "SELECT id FROM runtime_tasks WHERE kind=$1 AND idempotency_key=$2", input.Kind, input.IdempotencyKey).Scan(&inserted); err != nil {
		return "", false, err
	}
	return inserted, true, nil
}

func (r *RuntimeTaskRepository) Get(ctx context.Context, id string) (*RuntimeTask, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("runtime task id is required")
	}
	row := r.pg.Pool.QueryRow(ctx, runtimeTaskSelect+" WHERE id=$1", id)
	return scanRuntimeTask(row)
}

func (r *RuntimeTaskRepository) Claim(ctx context.Context, owner string, kinds []string, ttl time.Duration) (*ClaimedRuntimeTask, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("runtime task lease owner is required")
	}
	if len(kinds) == 0 {
		return nil, errors.New("at least one runtime task kind is required")
	}
	for _, kind := range kinds {
		if !runtimeTaskKindPattern.MatchString(kind) {
			return nil, errors.New("runtime task kind must be a lowercase slug")
		}
	}
	if ttl <= 0 {
		return nil, errors.New("runtime task lease ttl must be positive")
	}
	now := time.Now().UnixMilli()
	token := uuid.NewString()
	row := r.pg.Pool.QueryRow(ctx, `UPDATE runtime_tasks SET
status='running',attempts=attempts+1,lease_token=$1,lease_owner=$2,lease_expires_at=$3,
started_at=COALESCE(started_at,$4),updated_at=$4
WHERE id=(SELECT candidate.id FROM runtime_tasks candidate
  WHERE candidate.status='pending' AND candidate.available_at <= $4 AND candidate.kind=ANY($5)
    AND (candidate.serial_key IS NULL OR NOT EXISTS (
      SELECT 1 FROM runtime_tasks active WHERE active.serial_key=candidate.serial_key AND active.status='running'
    ))
  ORDER BY candidate.priority DESC,candidate.available_at ASC,candidate.created_at ASC
  FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id,kind,payload_version,payload,scope_id,actor_id,idempotency_key,serial_key,status,priority,available_at,
attempts,max_attempts,lease_token,lease_owner,lease_expires_at,cancel_requested,result,last_error,created_at,updated_at,started_at,finished_at`,
		token, owner, now+ttl.Milliseconds(), now, kinds)
	task, err := scanRuntimeTask(row)
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return nil, nil
	}
	if err != nil || task == nil {
		return nil, err
	}
	return &ClaimedRuntimeTask{RuntimeTask: *task, ClaimedLeaseToken: token, ClaimedLeaseOwner: owner}, nil
}

func (r *RuntimeTaskRepository) Heartbeat(ctx context.Context, id, leaseToken string, ttl time.Duration) (bool, bool, error) {
	if ttl <= 0 {
		return false, false, errors.New("runtime task lease ttl must be positive")
	}
	var cancelRequested bool
	err := r.pg.Pool.QueryRow(ctx, `UPDATE runtime_tasks SET lease_expires_at=$1,updated_at=$2
WHERE id=$3 AND lease_token=$4 AND status='running' RETURNING cancel_requested`,
		time.Now().Add(ttl).UnixMilli(), time.Now().UnixMilli(), id, leaseToken).Scan(&cancelRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, cancelRequested, err
}

func (r *RuntimeTaskRepository) AppendEvent(ctx context.Context, id, leaseToken string, event RuntimeTaskEvent) (bool, error) {
	if event.Sequence < 0 || strings.TrimSpace(event.Type) == "" || !json.Valid(event.Payload) {
		return false, errors.New("runtime task event sequence, type, and JSON payload are required")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var active bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_tasks WHERE id=$1 AND lease_token=$2 AND status='running')", id, leaseToken).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	createdAt := event.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runtime_task_events(task_id,sequence,type,payload,created_at)
VALUES($1,$2,$3,$4::jsonb,$5) ON CONFLICT(task_id,sequence) DO NOTHING`, id, event.Sequence, event.Type, string(event.Payload), createdAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *RuntimeTaskRepository) Complete(ctx context.Context, id, leaseToken string, result json.RawMessage) (bool, error) {
	if !json.Valid(result) {
		return false, errors.New("runtime task result must be valid JSON")
	}
	now := time.Now().UnixMilli()
	tag, err := r.pg.Pool.Exec(ctx, `UPDATE runtime_tasks SET status='succeeded',result=$1::jsonb,last_error=NULL,
lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=$2,finished_at=$2
WHERE id=$3 AND lease_token=$4 AND status='running'`, string(result), now, id, leaseToken)
	return tag.RowsAffected() == 1, err
}

func (r *RuntimeTaskRepository) Release(ctx context.Context, id, leaseToken string) (bool, error) {
	now := time.Now().UnixMilli()
	tag, err := r.pg.Pool.Exec(ctx, `UPDATE runtime_tasks SET status='pending',lease_token=NULL,lease_owner=NULL,
lease_expires_at=NULL,updated_at=$1 WHERE id=$2 AND lease_token=$3 AND status='running' AND cancel_requested=FALSE`,
		now, id, leaseToken)
	return tag.RowsAffected() == 1, err
}

func (r *RuntimeTaskRepository) AcknowledgeCancellation(ctx context.Context, id, leaseToken string) (bool, error) {
	now := time.Now().UnixMilli()
	tag, err := r.pg.Pool.Exec(ctx, `UPDATE runtime_tasks SET status='cancelled',lease_token=NULL,lease_owner=NULL,
lease_expires_at=NULL,updated_at=$1,finished_at=$1 WHERE id=$2 AND lease_token=$3 AND status='running' AND cancel_requested=TRUE`,
		now, id, leaseToken)
	return tag.RowsAffected() == 1, err
}

func (r *RuntimeTaskRepository) Fail(ctx context.Context, id, leaseToken, reason string, retry bool, delay time.Duration) (bool, bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var attempts, maxAttempts int
	if err := tx.QueryRow(ctx, `SELECT attempts,max_attempts FROM runtime_tasks
WHERE id=$1 AND lease_token=$2 AND status='running' FOR UPDATE`, id, leaseToken).Scan(&attempts, &maxAttempts); errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	now := time.Now().UnixMilli()
	if retry && attempts < maxAttempts {
		availableAt := time.Now().Add(delay).UnixMilli()
		if _, err := tx.Exec(ctx, `UPDATE runtime_tasks SET status='pending',available_at=$1,last_error=$2,
lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=$3 WHERE id=$4`, availableAt, reason, now, id); err != nil {
			return false, false, err
		}
		return true, true, tx.Commit(ctx)
	}
	status := "failed"
	if retry {
		status = "dead"
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime_tasks SET status=$1,last_error=$2,lease_token=NULL,lease_owner=NULL,
lease_expires_at=NULL,updated_at=$3,finished_at=$3 WHERE id=$4`, status, reason, now, id); err != nil {
		return false, false, err
	}
	return true, false, tx.Commit(ctx)
}

func (r *RuntimeTaskRepository) Cancel(ctx context.Context, id string) (bool, error) {
	now := time.Now().UnixMilli()
	tag, err := r.pg.Pool.Exec(ctx, `UPDATE runtime_tasks SET
status=CASE WHEN status='pending' THEN 'cancelled' ELSE status END,
cancel_requested=CASE WHEN status='running' THEN TRUE ELSE cancel_requested END,
finished_at=CASE WHEN status='pending' THEN $1 ELSE finished_at END,updated_at=$1
WHERE id=$2 AND status IN ('pending','running')`, now, id)
	return tag.RowsAffected() == 1, err
}

func (r *RuntimeTaskRepository) ReapExpired(ctx context.Context) (int64, int64, error) {
	now := time.Now().UnixMilli()
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	requeued, err := tx.Exec(ctx, `UPDATE runtime_tasks SET status='pending',lease_token=NULL,lease_owner=NULL,
lease_expires_at=NULL,last_error='lease expired',updated_at=$1
WHERE status='running' AND lease_expires_at <= $1 AND cancel_requested=FALSE AND attempts < max_attempts`, now)
	if err != nil {
		return 0, 0, err
	}
	dead, err := tx.Exec(ctx, `UPDATE runtime_tasks SET status=CASE WHEN cancel_requested THEN 'cancelled' ELSE 'dead' END,
lease_token=NULL,lease_owner=NULL,lease_expires_at=NULL,last_error=CASE WHEN cancel_requested THEN last_error ELSE 'lease expired' END,
updated_at=$1,finished_at=$1
WHERE status='running' AND lease_expires_at <= $1 AND (cancel_requested=TRUE OR attempts >= max_attempts)`, now)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return requeued.RowsAffected(), dead.RowsAffected(), nil
}

func (r *RuntimeTaskRepository) ListEvents(ctx context.Context, id string) ([]RuntimeTaskEvent, error) {
	return r.ListEventsAfter(ctx, id, -1)
}

func (r *RuntimeTaskRepository) ListEventsAfter(ctx context.Context, id string, after int64) ([]RuntimeTaskEvent, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT sequence,type,payload,created_at FROM runtime_task_events
WHERE task_id=$1 AND sequence > $2 ORDER BY sequence`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RuntimeTaskEvent
	for rows.Next() {
		var event RuntimeTaskEvent
		if err := rows.Scan(&event.Sequence, &event.Type, &event.Payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

const runtimeTaskSelect = `SELECT id,kind,payload_version,payload,scope_id,actor_id,idempotency_key,serial_key,status,priority,available_at,
attempts,max_attempts,lease_token,lease_owner,lease_expires_at,cancel_requested,result,last_error,created_at,updated_at,started_at,finished_at
FROM runtime_tasks`

type runtimeTaskScanner interface {
	Scan(dest ...any) error
}

func scanRuntimeTask(row runtimeTaskScanner) (*RuntimeTask, error) {
	var task RuntimeTask
	err := row.Scan(&task.ID, &task.Kind, &task.PayloadVersion, &task.Payload, &task.ScopeID, &task.ActorID, &task.IdempotencyKey,
		&task.SerialKey, &task.Status, &task.Priority, &task.AvailableAt, &task.Attempts, &task.MaxAttempts, &task.LeaseToken, &task.LeaseOwner,
		&task.LeaseExpiresAt, &task.CancelRequested, &task.Result, &task.LastError, &task.CreatedAt, &task.UpdatedAt,
		&task.StartedAt, &task.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}
