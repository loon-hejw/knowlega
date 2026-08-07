package data

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ClaimedRun struct {
	ID             string
	SessionID      string
	LeaseToken     string
	Payload        []byte
	Attempt        int32
	LeaseExpiresAt int64
	MaxAttempts    int32
	ErrorAttempts  int32
	CreatedAt      int64
	StartedAt      int64
}

// RunSnapshot is the durable portion of a run. It deliberately excludes Node's
// in-memory stream state while preserving every persisted field needed by the
// shared queue clients.
type RunSnapshot struct {
	ID             string
	SessionID      string
	Status         string
	Payload        []byte
	Result         []byte
	DeliveryState  []byte
	IdempotencyKey string
	Attempts       int32
	ErrorAttempts  int32
	MaxAttempts    int32
	LeaseToken     string
	LeaseExpiresAt *int64
	WorkerID       string
	CreatedAt      int64
	StartedAt      *int64
	FinishedAt     *int64
}

type RunRepository struct{ pg *Postgres }

type AdminRun struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	SessionScope   *string `json:"sessionScope"`
	SessionType    *string `json:"sessionType"`
	ThreadRef      string  `json:"threadRef"`
	Attempts       int     `json:"attempts"`
	MaxAttempts    int     `json:"maxAttempts"`
	WorkerID       *string `json:"workerId"`
	LeaseExpiresAt *int64  `json:"leaseExpiresAt"`
	CreatedAt      int64   `json:"createdAt"`
	StartedAt      *int64  `json:"startedAt"`
	FinishedAt     *int64  `json:"finishedAt"`
}

type RunSignal struct {
	ID        int64
	Kind      string
	Text      string
	Payload   []byte
	CreatedAt int64
}

type RunActivity struct {
	Sequence       int64
	ParentSequence *int64
	Type           string
	Payload        []byte
	CreatedAt      int64
}

type ReapEvent struct {
	RunID         string
	SessionID     string
	WorkerID      string
	Attempts      int32
	ErrorAttempts int32
	Outcome       string
}

func NewRunRepository(pg *Postgres) *RunRepository { return &RunRepository{pg: pg} }

func (r *RunRepository) ActiveForSession(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := r.pg.Pool.QueryRow(ctx, `SELECT id FROM runs WHERE session_id=$1 AND status IN ('pending','running') ORDER BY created_at DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (r *RunRepository) SetDeliveryState(ctx context.Context, runID, editRef string) (bool, error) {
	state, err := json.Marshal(map[string]string{"editRef": editRef})
	if err != nil {
		return false, err
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "UPDATE runs SET delivery_state=$1 WHERE id=$2", string(state), runID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Rollback(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET destination = jsonb_set(destination, '{editRef}', to_jsonb($2::text))
WHERE idempotency_key=$1 AND delivered_at IS NULL`, "run:"+runID, editRef); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *RunRepository) ListAdmin(ctx context.Context, scope string, orgWide bool) ([]AdminRun, error) {
	return r.listAdmin(ctx, scope, orgWide, 200)
}

func (r *RunRepository) ListAdminMetrics(ctx context.Context, scope string, orgWide bool) ([]AdminRun, error) {
	return r.listAdmin(ctx, scope, orgWide, 500)
}

func (r *RunRepository) listAdmin(ctx context.Context, scope string, orgWide bool, limit int) ([]AdminRun, error) {
	rows, err := r.pg.Pool.Query(ctx, `WITH candidates AS (
  SELECT id,session_id,status,attempts,max_attempts,worker_id,lease_expires_at,created_at,started_at,finished_at
  FROM runs
  ORDER BY CASE WHEN status IN ('pending','running') THEN 0 ELSE 1 END,created_at DESC
  LIMIT $1
)
SELECT c.id,c.status,s.scope_id,s.type,c.session_id,c.attempts,c.max_attempts,c.worker_id,c.lease_expires_at,c.created_at,c.started_at,c.finished_at
FROM candidates c LEFT JOIN sessions s ON s.thread_ref=c.session_id
ORDER BY CASE WHEN c.status IN ('pending','running') THEN 0 ELSE 1 END,c.created_at DESC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AdminRun{}
	for rows.Next() {
		var item AdminRun
		if err := rows.Scan(&item.ID, &item.Status, &item.SessionScope, &item.SessionType, &item.ThreadRef, &item.Attempts, &item.MaxAttempts, &item.WorkerID, &item.LeaseExpiresAt, &item.CreatedAt, &item.StartedAt, &item.FinishedAt); err != nil {
			return nil, err
		}
		if orgWide || item.SessionScope != nil && *item.SessionScope == scope {
			result = append(result, item)
		}
	}
	return result, rows.Err()
}

func (r *RunRepository) Enqueue(ctx context.Context, sessionID string, payload json.RawMessage, idempotencyKey string, maxAttempts int) (string, bool, error) {
	if sessionID == "" || !json.Valid(payload) {
		return "", false, errors.New("session_id and JSON payload are required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	id := uuid.NewString()
	row := r.pg.Pool.QueryRow(ctx, `INSERT INTO runs(id,session_id,status,request,idempotency_key,attempts,max_attempts,created_at)
VALUES($1,$2,'pending',$3,$4,0,$5,$6) ON CONFLICT(idempotency_key) DO NOTHING RETURNING id`, id, sessionID, string(payload), nullIfEmpty(idempotencyKey), maxAttempts, time.Now().UnixMilli())
	var inserted string
	if err := row.Scan(&inserted); err == nil {
		return inserted, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	if idempotencyKey == "" {
		return "", false, errors.New("enqueue conflict without idempotency key")
	}
	if err := r.pg.Pool.QueryRow(ctx, "SELECT id FROM runs WHERE idempotency_key=$1", idempotencyKey).Scan(&inserted); err != nil {
		return "", false, err
	}
	return inserted, true, nil
}

func (r *RunRepository) Get(ctx context.Context, runID string) (*RunSnapshot, error) {
	if runID == "" {
		return nil, errors.New("run_id is required")
	}
	var run RunSnapshot
	err := r.pg.Pool.QueryRow(ctx, `SELECT id,session_id,status,request,result,delivery_state,COALESCE(idempotency_key,''),
attempts,error_attempts,max_attempts,COALESCE(lease_token,''),lease_expires_at,COALESCE(worker_id,''),created_at,started_at,finished_at
FROM runs WHERE id=$1`, runID).Scan(&run.ID, &run.SessionID, &run.Status, &run.Payload, &run.Result, &run.DeliveryState, &run.IdempotencyKey,
		&run.Attempts, &run.ErrorAttempts, &run.MaxAttempts, &run.LeaseToken, &run.LeaseExpiresAt, &run.WorkerID, &run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *RunRepository) Active(ctx context.Context, sessionID string) (*RunSnapshot, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}
	var run RunSnapshot
	err := r.pg.Pool.QueryRow(ctx, `SELECT id,session_id,status,request,result,delivery_state,COALESCE(idempotency_key,''),
attempts,error_attempts,max_attempts,COALESCE(lease_token,''),lease_expires_at,COALESCE(worker_id,''),created_at,started_at,finished_at
FROM runs WHERE session_id=$1 AND status IN ('pending','running') ORDER BY created_at DESC LIMIT 1`, sessionID).Scan(&run.ID, &run.SessionID, &run.Status, &run.Payload, &run.Result, &run.DeliveryState, &run.IdempotencyKey,
		&run.Attempts, &run.ErrorAttempts, &run.MaxAttempts, &run.LeaseToken, &run.LeaseExpiresAt, &run.WorkerID, &run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *RunRepository) ActiveSessionIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT DISTINCT session_id FROM runs WHERE status IN ('pending','running') ORDER BY session_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *RunRepository) Claim(ctx context.Context, workerID string, ttl time.Duration) (*ClaimedRun, error) {
	if workerID == "" {
		return nil, errors.New("runner_id is required")
	}
	now := time.Now().UnixMilli()
	token := uuid.NewString()
	var run ClaimedRun
	err := r.pg.Pool.QueryRow(ctx, `UPDATE runs SET status='running',lease_token=$1,lease_expires_at=$2,worker_id=$3,
attempts=attempts+1,started_at=COALESCE(started_at,$4)
WHERE id=(SELECT id FROM runs WHERE status='pending' AND session_id NOT IN (SELECT session_id FROM runs WHERE status='running') ORDER BY created_at ASC FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id,session_id,lease_token,request,attempts,lease_expires_at,max_attempts,error_attempts,created_at,started_at`, token, now+ttl.Milliseconds(), workerID, now).Scan(&run.ID, &run.SessionID, &run.LeaseToken, &run.Payload, &run.Attempt, &run.LeaseExpiresAt, &run.MaxAttempts, &run.ErrorAttempts, &run.CreatedAt, &run.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// ClaimByID is the inline-turn counterpart to Claim. It preserves the
// one-running-run-per-session guard, so a synchronous caller cannot jump ahead
// of another active turn for the same conversation.
func (r *RunRepository) ClaimByID(ctx context.Context, runID, workerID string, ttl time.Duration) (*ClaimedRun, error) {
	if runID == "" || workerID == "" {
		return nil, errors.New("run_id and runner_id are required")
	}
	now := time.Now().UnixMilli()
	token := uuid.NewString()
	var run ClaimedRun
	err := r.pg.Pool.QueryRow(ctx, `UPDATE runs SET status='running',lease_token=$1,lease_expires_at=$2,worker_id=$3,
attempts=attempts+1,started_at=COALESCE(started_at,$4)
WHERE id=(SELECT id FROM runs WHERE id=$5 AND status='pending'
  AND session_id NOT IN (SELECT session_id FROM runs WHERE status='running')
  FOR UPDATE SKIP LOCKED)
RETURNING id,session_id,lease_token,request,attempts,lease_expires_at,max_attempts,error_attempts,created_at,started_at`, token, now+ttl.Milliseconds(), workerID, now, runID).Scan(&run.ID, &run.SessionID, &run.LeaseToken, &run.Payload, &run.Attempt, &run.LeaseExpiresAt, &run.MaxAttempts, &run.ErrorAttempts, &run.CreatedAt, &run.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *RunRepository) Heartbeat(ctx context.Context, runID, leaseToken string, ttl time.Duration) (bool, error) {
	tag, err := r.pg.Pool.Exec(ctx, "UPDATE runs SET lease_expires_at=$1 WHERE id=$2 AND lease_token=$3 AND status='running'", time.Now().Add(ttl).UnixMilli(), runID, leaseToken)
	return tag.RowsAffected() == 1, err
}

func (r *RunRepository) ReleaseLease(ctx context.Context, runID, leaseToken string) (bool, error) {
	tag, err := r.pg.Pool.Exec(ctx, "UPDATE runs SET status='pending',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL WHERE id=$1 AND lease_token=$2 AND status='running'", runID, leaseToken)
	return tag.RowsAffected() == 1, err
}

func (r *RunRepository) Fail(ctx context.Context, runID, leaseToken, reason string, retry bool) (bool, bool, error) {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var sessionID string
	var errorAttempts, maxAttempts int
	err = tx.QueryRow(ctx, "SELECT session_id,error_attempts,max_attempts FROM runs WHERE id=$1 AND lease_token=$2 AND status='running' FOR UPDATE", runID, leaseToken).Scan(&sessionID, &errorAttempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if retry && errorAttempts+1 < maxAttempts {
		_, err = tx.Exec(ctx, "UPDATE runs SET status='pending',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,error_attempts=error_attempts+1 WHERE id=$1", runID)
		if err != nil {
			return false, false, err
		}
		return true, true, tx.Commit(ctx)
	}
	result, err := json.Marshal(map[string]string{"status": "failed", "sessionId": sessionID, "reason": reason})
	if err != nil {
		return false, false, err
	}
	_, err = tx.Exec(ctx, "UPDATE runs SET status='failed',result=$1,lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,finished_at=$2,error_attempts=error_attempts+1 WHERE id=$3", string(result), time.Now().UnixMilli(), runID)
	if err != nil {
		return false, false, err
	}
	return true, false, tx.Commit(ctx)
}

func (r *RunRepository) AppendEvent(ctx context.Context, runID, leaseToken string, sequence int64, key string, payload json.RawMessage) (bool, error) {
	if sequence < 0 || key == "" || !json.Valid(payload) {
		return false, errors.New("sequence, idempotency_key, and JSON payload are required")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var active bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1 AND lease_token=$2 AND status='running')", runID, leaseToken).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO run_activity(run_id,seq,parent_seq,type,payload,created_at,idempotency_key)
VALUES($1,$2,NULL,'runner_event',$3::jsonb,$4,$5) ON CONFLICT(run_id,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, runID, sequence, string(payload), time.Now().UnixMilli(), key)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (r *RunRepository) Complete(ctx context.Context, runID, leaseToken, key string, result json.RawMessage) (bool, error) {
	if key == "" || !json.Valid(result) {
		return false, errors.New("idempotency_key and JSON result are required")
	}
	tag, err := r.pg.Pool.Exec(ctx, `UPDATE runs SET status='done',result=$1,completion_key=$2,lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,finished_at=$3
WHERE id=$4 AND lease_token=$5 AND status='running' AND (completion_key IS NULL OR completion_key=$2)`, string(result), key, time.Now().UnixMilli(), runID, leaseToken)
	if err != nil || tag.RowsAffected() == 1 {
		return tag.RowsAffected() == 1, err
	}
	var same bool
	err = r.pg.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1 AND status='done' AND completion_key=$2)", runID, key).Scan(&same)
	return same, err
}

func (r *RunRepository) Signal(ctx context.Context, runID string, payload json.RawMessage) (bool, error) {
	if runID == "" || !json.Valid(payload) {
		return false, errors.New("run_id and JSON signal payload are required")
	}
	var signal struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &signal); err != nil || (signal.Kind != "abort" && signal.Kind != "steer") {
		return false, errors.New("signal kind must be abort or steer")
	}
	_, err := r.pg.Pool.Exec(ctx, "INSERT INTO run_signals(run_id,kind,text,payload,created_at) VALUES($1,$2,NULLIF($3,''),$4::jsonb,$5)", runID, signal.Kind, signal.Text, string(payload), time.Now().UnixMilli())
	return err == nil, err
}

// SignalIfActive mirrors the control-plane signal contract: a signal can only
// be accepted for an existing pending or running run. Holding the row lock
// keeps a terminal transition from being interleaved with an accepted signal.
func (r *RunRepository) SignalIfActive(ctx context.Context, runID string, payload json.RawMessage) (string, error) {
	if runID == "" || !json.Valid(payload) {
		return "", errors.New("run_id and JSON signal payload are required")
	}
	var signal struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &signal); err != nil || (signal.Kind != "abort" && signal.Kind != "steer") {
		return "", errors.New("signal kind must be abort or steer")
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1 FOR UPDATE", runID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return "not_found", nil
	} else if err != nil {
		return "", err
	}
	if status == "done" || status == "failed" {
		return "terminal", nil
	}
	if _, err := tx.Exec(ctx, "INSERT INTO run_signals(run_id,kind,text,payload,created_at) VALUES($1,$2,NULLIF($3,''),$4::jsonb,$5)", runID, signal.Kind, signal.Text, string(payload), time.Now().UnixMilli()); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return "accepted", nil
}

func (r *RunRepository) TakePendingSignals(ctx context.Context, runID string) ([]RunSignal, error) {
	rows, err := r.pg.Pool.Query(ctx, `UPDATE run_signals SET consumed_at=$2 WHERE run_id=$1 AND consumed_at IS NULL
RETURNING id,kind,COALESCE(text,''),payload,created_at`, runID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var signals []RunSignal
	for rows.Next() {
		var signal RunSignal
		if err := rows.Scan(&signal.ID, &signal.Kind, &signal.Text, &signal.Payload, &signal.CreatedAt); err != nil {
			return nil, err
		}
		signals = append(signals, signal)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(signals, func(i, j int) bool { return signals[i].ID < signals[j].ID })
	return signals, nil
}

func (r *RunRepository) PendingSignalRunIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT DISTINCT run_id FROM run_signals WHERE consumed_at IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *RunRepository) PruneSignals(ctx context.Context, olderThan int64) (int64, error) {
	tag, err := r.pg.Pool.Exec(ctx, "DELETE FROM run_signals WHERE consumed_at IS NOT NULL AND consumed_at < $1", olderThan)
	return tag.RowsAffected(), err
}

func (r *RunRepository) AppendActivity(ctx context.Context, runID string, activity RunActivity) (bool, error) {
	if runID == "" || activity.Type == "" || !json.Valid(activity.Payload) {
		return false, errors.New("run_id, activity type, and JSON payload are required")
	}
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO run_activity(run_id,seq,parent_seq,type,payload,created_at)
VALUES($1,$2,$3,$4,$5::jsonb,$6)`, runID, activity.Sequence, activity.ParentSequence, activity.Type, string(activity.Payload), activity.CreatedAt)
	return err == nil, err
}

func (r *RunRepository) ListActivity(ctx context.Context, runID string) ([]RunActivity, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT seq,parent_seq,type,payload,created_at FROM run_activity WHERE run_id=$1 ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var activities []RunActivity
	for rows.Next() {
		var activity RunActivity
		if err := rows.Scan(&activity.Sequence, &activity.ParentSequence, &activity.Type, &activity.Payload, &activity.CreatedAt); err != nil {
			return nil, err
		}
		activities = append(activities, activity)
	}
	return activities, rows.Err()
}

func (r *RunRepository) PruneActivity(ctx context.Context, olderThan int64) (int64, error) {
	tag, err := r.pg.Pool.Exec(ctx, "DELETE FROM run_activity WHERE created_at < $1", olderThan)
	return tag.RowsAffected(), err
}

func (r *RunRepository) ReapExpired(ctx context.Context, maxAge time.Duration, maxClaims int) ([]ReapEvent, error) {
	now := time.Now().UnixMilli()
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,session_id,COALESCE(worker_id,''),attempts,error_attempts,max_attempts,lease_token,started_at
FROM runs WHERE status='running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []struct {
		id, sessionID, workerID, leaseToken  string
		attempts, errorAttempts, maxAttempts int32
		startedAt                            *int64
	}
	for rows.Next() {
		var candidate struct {
			id, sessionID, workerID, leaseToken  string
			attempts, errorAttempts, maxAttempts int32
			startedAt                            *int64
		}
		if err := rows.Scan(&candidate.id, &candidate.sessionID, &candidate.workerID, &candidate.attempts, &candidate.errorAttempts, &candidate.maxAttempts, &candidate.leaseToken, &candidate.startedAt); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	events := make([]ReapEvent, 0, len(candidates))
	for _, candidate := range candidates {
		tooOld := maxAge > 0 && candidate.startedAt != nil && now-*candidate.startedAt > maxAge.Milliseconds()
		retry := !tooOld
		overClaimed := maxClaims > 0 && int(candidate.attempts) >= maxClaims
		outcome := "requeued"
		var applied bool
		if retry && candidate.errorAttempts+1 < candidate.maxAttempts && !overClaimed {
			tag, err := r.pg.Pool.Exec(ctx, `UPDATE runs SET status='pending',lease_token=NULL,lease_expires_at=NULL,worker_id=NULL
WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at <= $3`, candidate.id, candidate.leaseToken, now)
			if err != nil {
				return nil, err
			}
			applied = tag.RowsAffected() == 1
		} else {
			outcome = "parked"
			reason := "lease expired (reaped)"
			if tooOld {
				reason = "run exceeded max age (reaped)"
			} else if overClaimed && retry && candidate.errorAttempts+1 < candidate.maxAttempts {
				reason = "run parked after " + strconv.Itoa(int(candidate.attempts)) + " claims without completing (suspected crash loop)"
			}
			result, err := json.Marshal(map[string]string{"status": "failed", "sessionId": candidate.sessionID, "reason": reason})
			if err != nil {
				return nil, err
			}
			tag, err := r.pg.Pool.Exec(ctx, `UPDATE runs SET status='failed',result=$1,lease_token=NULL,lease_expires_at=NULL,worker_id=NULL,finished_at=$2
WHERE id=$3 AND lease_token=$4 AND status='running' AND lease_expires_at <= $5`, string(result), now, candidate.id, candidate.leaseToken, now)
			if err != nil {
				return nil, err
			}
			applied = tag.RowsAffected() == 1
		}
		if applied {
			events = append(events, ReapEvent{RunID: candidate.id, SessionID: candidate.sessionID, WorkerID: candidate.workerID, Attempts: candidate.attempts, ErrorAttempts: candidate.errorAttempts, Outcome: outcome})
		}
	}
	return events, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
