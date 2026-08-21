package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SessionRepository struct{ pg *Postgres }

type SessionScopeMismatchError struct {
	ThreadRef       string
	ExpectedType    string
	ReceivedType    string
	ExpectedScopeID string
	ReceivedScopeID string
}

func (e *SessionScopeMismatchError) Error() string {
	if e == nil {
		return "session scope mismatch"
	}
	return fmt.Sprintf("thread %s belongs to %s scope %s, not %s scope %s", e.ThreadRef, e.ExpectedType, e.ExpectedScopeID, e.ReceivedType, e.ReceivedScopeID)
}

type SessionSummary struct {
	ID, Type, ScopeID, ThreadRef string
	Turns, Messages              int
	LastActivity, CreatedAt      int64
	FirstMessage, LastMessage    string
}

type SessionStats struct {
	Total, Turns, Crons int
	ByType, ByTypeAll   map[string]int
	TotalByCategory     map[string]int
}

type SessionRecord struct {
	ID, Type, ScopeID, ThreadRef string
	CreatedAt                    int64
	Surface, Title, ChannelName  *string
}

type DistinctSessionScope struct {
	ScopeID     string
	ChannelName *string
}

// ViewerSession is the session shape augmented with the viewer-specific state
// held on its participant record.  A participant can still see the session
// after being removed, but its transcript is constrained by the participant
// window.
type ViewerSession struct {
	SessionRecord
	ParticipantTitle, Color            *string
	Archived, Pinned, HasEntries       bool
	LastActivity                       int64
	ForkedFromSession, ForkedFromTitle *string
	ForkBoundarySeq                    *int
}

type SessionViewPatch struct {
	TitleSet, ArchivedSet, PinnedSet, ColorSet bool
	Title, Color                               *string
	Archived, Pinned                           bool
}

type SessionRuntimeState struct {
	WorkingThreadRefs map[string]bool
	AwaitingSessions  map[string]bool
	BackgroundJobs    map[string]int
	Watches           map[string]int
}

type BackgroundJob struct {
	ProcessID, Command   string
	StartedAt, ExpiresAt int64
}

type SessionWatch struct {
	ID, ProcessID, Command string
	Pattern, Instructions  *string
	CreatedAt, ExpiresAt   int64
	LastFiredAt            *int64
}

type PendingApproval struct {
	ID, SessionID, Command, Reason, Matched, Purpose, Summary, Kind, ActorID string
	ScopeVersion, ApprovalKey                                                string
	CreatedAt                                                                *int64
	GrantModes                                                               json.RawMessage
	Raw                                                                      json.RawMessage
	BlocksInput                                                              bool
}

type LLMRequest struct {
	ID, SessionID, Model, ScopeLabel        string
	TurnSeq                                 *int
	Step                                    int
	CreatedAt                               int64
	Truncated                               bool
	TTFTMS, DurationMS, StepGapMS           *int
	ToolWallMS, Usage, Transport, GapPhases json.RawMessage
	Request                                 json.RawMessage
}

type NewLLMRequest struct {
	TurnSeq                                 *int
	Step                                    int
	Model, ScopeLabel                       string
	Request                                 json.RawMessage
	Truncated                               bool
	TTFTMS, DurationMS, StepGapMS           *int
	ToolWallMS, Usage, Transport, GapPhases json.RawMessage
}

type SessionEntry struct {
	SessionID, Type, ScopeLabel string
	Sequence                    int
	ParentSequence              *int
	Payload                     json.RawMessage
	CreatedAt                   int64
}

type NewSessionEntry struct {
	Type, ScopeLabel string
	Payload          json.RawMessage
}

type NewTapeRecord struct {
	Kind, Harness, ScopeLabel     string
	Payload                       json.RawMessage
	BareText, TS, ChangeTime      *string
	Hidden, Overheard             *bool
	Author                        *string
	EntrySequence, CoversEntrySeq *int
}

type TapeRecord struct {
	NewTapeRecord
	SessionID string
	Sequence  int
	CreatedAt int64
}

type ParticipantWindow struct {
	SessionID, PrincipalID string
	ValidFrom              int64
	ValidTo                *int64
}
type AttributedTurn struct {
	PrincipalID, SessionID string
	Turns                  int
	FirstAt, LastAt        int64
}

func NewSessionRepository(pg *Postgres) *SessionRepository { return &SessionRepository{pg: pg} }

func (r *SessionRepository) GetOrCreateByThread(ctx context.Context, threadRef, sessionType, scopeID, channelName, surface string) (*SessionRecord, error) {
	if strings.TrimSpace(threadRef) == "" || strings.TrimSpace(sessionType) == "" || strings.TrimSpace(scopeID) == "" {
		return nil, errors.New("thread ref, session type, and scope id are required")
	}
	now := time.Now().UnixMilli()
	id := uuid.NewString()
	var item SessionRecord
	err := r.pg.Pool.QueryRow(ctx, `INSERT INTO sessions(id,type,scope_id,thread_ref,created_at,channel_name,surface,last_activity,messages,turns)
VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$5,0,0)
ON CONFLICT(thread_ref) DO UPDATE
SET channel_name=COALESCE(NULLIF(EXCLUDED.channel_name,''),sessions.channel_name),surface=COALESCE(sessions.surface,NULLIF(EXCLUDED.surface,''))
WHERE sessions.type=EXCLUDED.type AND sessions.scope_id=EXCLUDED.scope_id
RETURNING id,type,scope_id,thread_ref,created_at,surface,title,channel_name`, id, sessionType, scopeID, threadRef, now, channelName, surface).
		Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := r.GetByThread(ctx, threadRef)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if existing == nil {
			return nil, errors.New("session disappeared while checking its scope")
		}
		return nil, &SessionScopeMismatchError{
			ThreadRef: threadRef, ExpectedType: existing.Type, ReceivedType: sessionType,
			ExpectedScopeID: existing.ScopeID, ReceivedScopeID: scopeID,
		}
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *SessionRepository) AddParticipant(ctx context.Context, sessionID, principalID string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(principalID) == "" {
		return errors.New("session id and principal id are required")
	}
	_, err := r.pg.Pool.Exec(ctx, `INSERT INTO participants(session_id,principal_id,valid_from) VALUES($1,$2,$3) ON CONFLICT(session_id,principal_id) DO UPDATE SET valid_to=NULL,valid_to_seq=NULL`, sessionID, principalID, time.Now().UnixMilli())
	return err
}

func (r *SessionRepository) AppendEntry(ctx context.Context, sessionID string, entry NewSessionEntry) (SessionEntry, error) {
	if r == nil || r.pg == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(entry.Type) == "" || strings.TrimSpace(entry.ScopeLabel) == "" {
		return SessionEntry{}, errors.New("session id, entry type, and scope label are required")
	}
	payload, err := normalizedJSON(entry.Payload)
	if err != nil {
		return SessionEntry{}, fmt.Errorf("session entry payload: %w", err)
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return SessionEntry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('session-entry'),hashtext($1))", sessionID); err != nil {
		return SessionEntry{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)", sessionID).Scan(&exists); err != nil {
		return SessionEntry{}, err
	}
	if !exists {
		return SessionEntry{}, fmt.Errorf("session %s does not exist", sessionID)
	}
	var sequence int
	if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(seq),-1)+1 FROM session_entries WHERE session_id=$1", sessionID).Scan(&sequence); err != nil {
		return SessionEntry{}, err
	}
	var parent *int
	if sequence > 0 {
		value := sequence - 1
		parent = &value
	}
	createdAt := time.Now().UnixMilli()
	if _, err := tx.Exec(ctx, `INSERT INTO session_entries(session_id,seq,parent_seq,type,payload,scope_label,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7)`, sessionID, sequence, parent, entry.Type, string(payload), entry.ScopeLabel, createdAt); err != nil {
		return SessionEntry{}, err
	}
	userTurn := entry.Type == "user" && !strings.Contains(string(payload), `"overheard":true`)
	turnIncrement := 0
	if userTurn {
		turnIncrement = 1
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions
SET last_activity=GREATEST(COALESCE(last_activity,0),$2),messages=$3,
turns=CASE WHEN turns IS NULL OR messages IS DISTINCT FROM $5
THEN (SELECT COUNT(*) FROM session_entries t WHERE t.session_id=$1 AND t.type='user' AND (t.payload IS NULL OR t.payload NOT LIKE '%"overheard":true%'))
ELSE turns+$4 END WHERE id=$1`, sessionID, createdAt, sequence+1, turnIncrement, sequence); err != nil {
		return SessionEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionEntry{}, err
	}
	return SessionEntry{SessionID: sessionID, Type: entry.Type, ScopeLabel: entry.ScopeLabel, Sequence: sequence, ParentSequence: parent, Payload: payload, CreatedAt: createdAt}, nil
}

func (r *SessionRepository) RecordLLMRequest(ctx context.Context, sessionID string, request NewLLMRequest) (LLMRequest, error) {
	if r == nil || r.pg == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.ScopeLabel) == "" || request.Step < 0 {
		return LLMRequest{}, errors.New("session id, model, scope label, and non-negative step are required")
	}
	requestJSON, err := normalizedJSON(request.Request)
	if err != nil {
		return LLMRequest{}, fmt.Errorf("llm request: %w", err)
	}
	optional := []json.RawMessage{request.ToolWallMS, request.Usage, request.Transport, request.GapPhases}
	for index := range optional {
		if len(optional[index]) != 0 && !json.Valid(optional[index]) {
			return LLMRequest{}, fmt.Errorf("llm request metadata %d is invalid JSON", index)
		}
	}
	stored := LLMRequest{
		ID: uuid.NewString(), SessionID: sessionID, TurnSeq: request.TurnSeq, Step: request.Step,
		Model: request.Model, ScopeLabel: request.ScopeLabel, Request: requestJSON, Truncated: request.Truncated,
		CreatedAt: time.Now().UnixMilli(), TTFTMS: request.TTFTMS, DurationMS: request.DurationMS, StepGapMS: request.StepGapMS,
		ToolWallMS: cloneJSON(request.ToolWallMS), Usage: cloneJSON(request.Usage), Transport: cloneJSON(request.Transport), GapPhases: cloneJSON(request.GapPhases),
	}
	_, err = r.pg.Pool.Exec(ctx, `INSERT INTO session_llm_requests(id,session_id,turn_seq,step,model,scope_label,request,truncated,created_at,ttft_ms,duration_ms,step_gap_ms,tool_wall_json,usage_json,transport_json,gap_phases_json)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),NULLIF($14,''),NULLIF($15,''),NULLIF($16,''))`,
		stored.ID, stored.SessionID, stored.TurnSeq, stored.Step, stored.Model, stored.ScopeLabel, string(stored.Request), stored.Truncated, stored.CreatedAt,
		stored.TTFTMS, stored.DurationMS, stored.StepGapMS, string(stored.ToolWallMS), string(stored.Usage), string(stored.Transport), string(stored.GapPhases))
	if err != nil {
		return LLMRequest{}, err
	}
	return stored, nil
}

func (r *SessionRepository) AppendTape(ctx context.Context, sessionID string, record NewTapeRecord) (TapeRecord, error) {
	if r == nil || r.pg == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(record.Kind) == "" || strings.TrimSpace(record.ScopeLabel) == "" {
		return TapeRecord{}, errors.New("session id, tape kind, and scope label are required")
	}
	payload, err := normalizedJSON(record.Payload)
	if err != nil {
		return TapeRecord{}, fmt.Errorf("session tape payload: %w", err)
	}
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return TapeRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('session-tape'),hashtext($1))", sessionID); err != nil {
		return TapeRecord{}, err
	}
	var sequence int
	if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(seq),-1)+1 FROM session_tape WHERE session_id=$1", sessionID).Scan(&sequence); err != nil {
		return TapeRecord{}, err
	}
	createdAt := time.Now().UnixMilli()
	if _, err := tx.Exec(ctx, `INSERT INTO session_tape(session_id,seq,kind,harness,payload,scope_label,bare_text,ts,change_time,hidden,overheard,author,entry_seq,covers_entry_seq,created_at)
VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		sessionID, sequence, record.Kind, record.Harness, string(payload), record.ScopeLabel, record.BareText, record.TS, record.ChangeTime,
		record.Hidden, record.Overheard, record.Author, record.EntrySequence, record.CoversEntrySeq, createdAt); err != nil {
		return TapeRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TapeRecord{}, err
	}
	record.Payload = payload
	return TapeRecord{NewTapeRecord: record, SessionID: sessionID, Sequence: sequence, CreatedAt: createdAt}, nil
}

func (r *SessionRepository) Tape(ctx context.Context, sessionID string, since int) ([]TapeRecord, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT session_id,seq,kind,COALESCE(harness,''),payload,scope_label,bare_text,ts,change_time,hidden,overheard,author,entry_seq,covers_entry_seq,created_at
FROM session_tape WHERE session_id=$1 AND seq>$2 ORDER BY seq`, sessionID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TapeRecord{}
	for rows.Next() {
		var record TapeRecord
		if err := rows.Scan(&record.SessionID, &record.Sequence, &record.Kind, &record.Harness, &record.Payload, &record.ScopeLabel,
			&record.BareText, &record.TS, &record.ChangeTime, &record.Hidden, &record.Overheard, &record.Author,
			&record.EntrySequence, &record.CoversEntrySeq, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (r *SessionRepository) TapeCoverage(ctx context.Context, sessionID string) (int, error) {
	if r == nil || r.pg == nil || strings.TrimSpace(sessionID) == "" {
		return -1, errors.New("session id is required")
	}
	var coverage int
	err := r.pg.Pool.QueryRow(ctx, `SELECT GREATEST(
  COALESCE(MAX(entry_seq) FILTER (
    WHERE kind='annotation' AND payload::jsonb->>'turnEnd'='true'
  ),-1),
  COALESCE(MAX(covers_entry_seq) FILTER (
    WHERE kind='context_event' AND payload::jsonb->>'event'='legacy_import'
  ),-1)
) FROM session_tape WHERE session_id=$1`, sessionID).Scan(&coverage)
	return coverage, err
}

func normalizedJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	return cloneJSON(raw), nil
}

func cloneJSON(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

// DistinctScopes and ParticipantPrincipalIDs support administrative scope
// discovery without reading session content.
func (r *SessionRepository) DistinctScopes(ctx context.Context) ([]DistinctSessionScope, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT scope_id,MAX(channel_name) FROM sessions GROUP BY scope_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DistinctSessionScope{}
	for rows.Next() {
		var scope DistinctSessionScope
		if err := rows.Scan(&scope.ScopeID, &scope.ChannelName); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}

func (r *SessionRepository) ParticipantPrincipalIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT DISTINCT principal_id FROM participants")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var principalID string
		if err := rows.Scan(&principalID); err != nil {
			return nil, err
		}
		result = append(result, principalID)
	}
	return result, rows.Err()
}

func (r *SessionRepository) Get(ctx context.Context, id string) (*SessionRecord, error) {
	var item SessionRecord
	err := r.pg.Pool.QueryRow(ctx, "SELECT id,type,scope_id,thread_ref,created_at,surface,title,channel_name FROM sessions WHERE id=$1", id).Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (r *SessionRepository) GetByThread(ctx context.Context, threadRef string) (*SessionRecord, error) {
	var item SessionRecord
	err := r.pg.Pool.QueryRow(ctx, "SELECT id,type,scope_id,thread_ref,created_at,surface,title,channel_name FROM sessions WHERE thread_ref=$1", threadRef).Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListByParticipant returns every session the participant has joined, whether
// the participant window is currently active or historic. It mirrors Node's
// administrative reset lookup, which must remove old personal sessions too.
func (r *SessionRepository) ListByParticipant(ctx context.Context, principalID string) ([]SessionRecord, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT s.id,s.type,s.scope_id,s.thread_ref,s.created_at,s.surface,s.title,s.channel_name
		FROM sessions s JOIN participants p ON p.session_id=s.id
		WHERE p.principal_id=$1 ORDER BY s.id`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionRecord{}
	for rows.Next() {
		var item SessionRecord
		if err := rows.Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// Delete removes exactly the session rows Node's PostgreSQL session store
// clears on an administrative reset. session_leases and session_tape are
// optional during a Go-first migration because they are provisioned by Node's
// session store; when present, they are removed in the same transaction.
func (r *SessionRepository) Delete(ctx context.Context, sessionID string) error {
	tx, err := r.pg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, table := range []string{"session_llm_requests", "session_leases", "participants", "session_entries", "session_tape"} {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE session_id=$1", sessionID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE id=$1", sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *SessionRepository) Entries(ctx context.Context, sessionID string, limit int, since int) ([]SessionEntry, error) {
	query := `SELECT session_id,seq,parent_seq,type,payload,scope_label,created_at FROM session_entries WHERE session_id=$1 AND seq >= $2 ORDER BY seq ASC`
	args := []any{sessionID, since}
	if limit > 0 {
		query = `SELECT session_id,seq,parent_seq,type,payload,scope_label,created_at FROM session_entries WHERE session_id=$1 AND seq >= $2 ORDER BY seq DESC LIMIT $3`
		args = append(args, limit)
	}
	rows, err := r.pg.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionEntry{}
	for rows.Next() {
		var item SessionEntry
		if err := rows.Scan(&item.SessionID, &item.Sequence, &item.ParentSequence, &item.Type, &item.Payload, &item.ScopeLabel, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if limit > 0 {
		sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	}
	return result, nil
}

func (r *SessionRepository) VisibleEntries(ctx context.Context, sessionID, principalID string) ([]SessionEntry, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT e.session_id,e.seq,e.parent_seq,e.type,e.payload,e.scope_label,e.created_at
		FROM session_entries e
		JOIN participants p ON p.session_id=e.session_id AND p.principal_id=$2
		WHERE e.session_id=$1
		  AND (((p.valid_from_seq IS NOT NULL AND e.seq>=p.valid_from_seq) OR (p.valid_from_seq IS NULL AND e.created_at>=p.valid_from))
		       AND ((p.valid_to_seq IS NOT NULL AND e.seq<p.valid_to_seq) OR (p.valid_to_seq IS NULL AND (p.valid_to IS NULL OR e.created_at<p.valid_to))))
		ORDER BY e.seq ASC`, sessionID, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionEntry{}
	for rows.Next() {
		var item SessionEntry
		if err := rows.Scan(&item.SessionID, &item.Sequence, &item.ParentSequence, &item.Type, &item.Payload, &item.ScopeLabel, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SessionRepository) ViewerSession(ctx context.Context, sessionID, principalID string) (*ViewerSession, error) {
	var item ViewerSession
	err := r.pg.Pool.QueryRow(ctx, `SELECT s.id,s.type,s.scope_id,s.thread_ref,s.created_at,s.surface,s.title,s.channel_name,
		p.title,p.color,p.archived,p.pinned,COALESCE(MAX(e.created_at),s.created_at),
		EXISTS (SELECT 1 FROM session_entries x WHERE x.session_id=s.id
		  AND (((p.valid_from_seq IS NOT NULL AND x.seq>=p.valid_from_seq) OR (p.valid_from_seq IS NULL AND x.created_at>=p.valid_from))
		       AND ((p.valid_to_seq IS NOT NULL AND x.seq<p.valid_to_seq) OR (p.valid_to_seq IS NULL AND (p.valid_to IS NULL OR x.created_at<p.valid_to))))),
		s.forked_from_session_id,s.forked_from_title,s.fork_boundary_seq
		FROM sessions s JOIN participants p ON p.session_id=s.id
		LEFT JOIN session_entries e ON e.session_id=s.id AND e.type='user'
		WHERE s.id=$1 AND p.principal_id=$2
		GROUP BY s.id,p.title,p.color,p.archived,p.pinned,p.valid_from,p.valid_to,p.valid_from_seq,p.valid_to_seq`, sessionID, principalID).
		Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName,
			&item.ParticipantTitle, &item.Color, &item.Archived, &item.Pinned, &item.LastActivity, &item.HasEntries,
			&item.ForkedFromSession, &item.ForkedFromTitle, &item.ForkBoundarySeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *SessionRepository) ListViewerSessions(ctx context.Context, principalID string) ([]ViewerSession, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT s.id,s.type,s.scope_id,s.thread_ref,s.created_at,s.surface,s.title,s.channel_name,
		p.title,p.color,p.archived,p.pinned,COALESCE(MAX(e.created_at),s.created_at),
		EXISTS (SELECT 1 FROM session_entries x WHERE x.session_id=s.id
		  AND (((p.valid_from_seq IS NOT NULL AND x.seq>=p.valid_from_seq) OR (p.valid_from_seq IS NULL AND x.created_at>=p.valid_from))
		       AND ((p.valid_to_seq IS NOT NULL AND x.seq<p.valid_to_seq) OR (p.valid_to_seq IS NULL AND (p.valid_to IS NULL OR x.created_at<p.valid_to))))),
		s.forked_from_session_id,s.forked_from_title,s.fork_boundary_seq
		FROM sessions s JOIN participants p ON p.session_id=s.id
		LEFT JOIN session_entries e ON e.session_id=s.id AND e.type='user'
		WHERE p.principal_id=$1
		GROUP BY s.id,p.title,p.color,p.archived,p.pinned,p.valid_from,p.valid_to,p.valid_from_seq,p.valid_to_seq`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ViewerSession{}
	for rows.Next() {
		var item ViewerSession
		if err := rows.Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.CreatedAt, &item.Surface, &item.Title, &item.ChannelName,
			&item.ParticipantTitle, &item.Color, &item.Archived, &item.Pinned, &item.LastActivity, &item.HasEntries,
			&item.ForkedFromSession, &item.ForkedFromTitle, &item.ForkBoundarySeq); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SessionRepository) UpdateViewerSession(ctx context.Context, sessionID, principalID string, patch SessionViewPatch) error {
	if patch.TitleSet {
		if _, err := r.pg.Pool.Exec(ctx, "UPDATE participants SET title=$3 WHERE session_id=$1 AND principal_id=$2", sessionID, principalID, patch.Title); err != nil {
			return err
		}
	}
	if patch.ArchivedSet {
		if _, err := r.pg.Pool.Exec(ctx, "UPDATE participants SET archived=$3 WHERE session_id=$1 AND principal_id=$2", sessionID, principalID, patch.Archived); err != nil {
			return err
		}
	}
	if patch.PinnedSet {
		if _, err := r.pg.Pool.Exec(ctx, "UPDATE participants SET pinned=$3 WHERE session_id=$1 AND principal_id=$2", sessionID, principalID, patch.Pinned); err != nil {
			return err
		}
	}
	if patch.ColorSet {
		if _, err := r.pg.Pool.Exec(ctx, "UPDATE participants SET color=$3 WHERE session_id=$1 AND principal_id=$2", sessionID, principalID, patch.Color); err != nil {
			return err
		}
	}
	return nil
}

func (r *SessionRepository) RuntimeState(ctx context.Context, now int64) (SessionRuntimeState, error) {
	state := SessionRuntimeState{WorkingThreadRefs: map[string]bool{}, AwaitingSessions: map[string]bool{}, BackgroundJobs: map[string]int{}, Watches: map[string]int{}}
	rows, err := r.pg.Pool.Query(ctx, "SELECT DISTINCT session_id FROM runs WHERE status IN ('pending','running')")
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var threadRef string
		if err := rows.Scan(&threadRef); err != nil {
			rows.Close()
			return state, err
		}
		state.WorkingThreadRefs[threadRef] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return state, err
	}
	rows.Close()
	if exists, err := r.tableExists(ctx, "process_sessions"); err != nil {
		return state, err
	} else if exists {
		rows, err = r.pg.Pool.Query(ctx, "SELECT session_ref FROM process_sessions WHERE kind='background' AND status='running' AND expires_at>$1 AND session_ref IS NOT NULL", now)
		if err != nil {
			return state, err
		}
		for rows.Next() {
			var threadRef string
			if err := rows.Scan(&threadRef); err != nil {
				rows.Close()
				return state, err
			}
			state.BackgroundJobs[threadRef]++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return state, err
		}
		rows.Close()
	}
	if exists, err := r.tableExists(ctx, "approvals"); err != nil {
		return state, err
	} else if exists {
		rows, err = r.pg.Pool.Query(ctx, "SELECT json FROM approvals")
		if err != nil {
			return state, err
		}
		for rows.Next() {
			var raw json.RawMessage
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return state, err
			}
			var item struct {
				SessionID   string `json:"sessionId"`
				BlocksInput *bool  `json:"blocksInput"`
			}
			if json.Unmarshal(raw, &item) == nil && item.SessionID != "" && (item.BlocksInput == nil || *item.BlocksInput) {
				state.AwaitingSessions[item.SessionID] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return state, err
		}
		rows.Close()
	}
	if exists, err := r.tableExists(ctx, "monitors"); err != nil {
		return state, err
	} else if exists {
		rows, err = r.pg.Pool.Query(ctx, "SELECT json FROM monitors")
		if err != nil {
			return state, err
		}
		for rows.Next() {
			var raw json.RawMessage
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return state, err
			}
			var item struct {
				ThreadRef string `json:"threadRef"`
				Enabled   bool   `json:"enabled"`
				ExpiresAt int64  `json:"expiresAt"`
			}
			if json.Unmarshal(raw, &item) == nil && item.Enabled && item.ExpiresAt > now && item.ThreadRef != "" {
				state.Watches[item.ThreadRef]++
			}
		}
		if err := rows.Err(); err != nil {
			return state, err
		}
		rows.Close()
	}
	return state, nil
}

func (r *SessionRepository) Background(ctx context.Context, threadRef string, now int64) ([]BackgroundJob, []SessionWatch, error) {
	jobs := []BackgroundJob{}
	if exists, err := r.tableExists(ctx, "process_sessions"); err != nil {
		return nil, nil, err
	} else if exists {
		rows, err := r.pg.Pool.Query(ctx, "SELECT process_id,command,started_at,expires_at FROM process_sessions WHERE kind='background' AND session_ref=$1 AND status='running' AND expires_at>$2 ORDER BY started_at DESC", threadRef, now)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var item BackgroundJob
			if err := rows.Scan(&item.ProcessID, &item.Command, &item.StartedAt, &item.ExpiresAt); err != nil {
				rows.Close()
				return nil, nil, err
			}
			jobs = append(jobs, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
	}
	watches := []SessionWatch{}
	if exists, err := r.tableExists(ctx, "monitors"); err != nil {
		return nil, nil, err
	} else if exists {
		rows, err := r.pg.Pool.Query(ctx, "SELECT json FROM monitors")
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var raw json.RawMessage
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, nil, err
			}
			var item struct {
				ID, ProcessID, Command, ThreadRef string
				Pattern, Instructions             *string
				CreatedAt, ExpiresAt              int64
				LastFiredAt                       *int64
				Enabled                           bool
			}
			if json.Unmarshal(raw, &item) == nil && item.Enabled && item.ThreadRef == threadRef && item.ExpiresAt > now {
				watches = append(watches, SessionWatch{ID: item.ID, ProcessID: item.ProcessID, Command: item.Command, Pattern: item.Pattern, Instructions: item.Instructions, CreatedAt: item.CreatedAt, ExpiresAt: item.ExpiresAt, LastFiredAt: item.LastFiredAt})
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		rows.Close()
	}
	sort.Slice(watches, func(i, j int) bool { return watches[i].CreatedAt > watches[j].CreatedAt })
	return jobs, watches, nil
}

func (r *SessionRepository) PendingApprovals(ctx context.Context, sessionID string) ([]PendingApproval, error) {
	if exists, err := r.tableExists(ctx, "approvals"); err != nil {
		return nil, err
	} else if !exists {
		return []PendingApproval{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, "SELECT id,json FROM approvals")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PendingApproval{}
	for rows.Next() {
		var id string
		var raw json.RawMessage
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var item struct {
			SessionID                                        string `json:"sessionId"`
			Command, Reason, Matched, Purpose, Summary, Kind string
			ApprovalKey                                      string          `json:"approvalKey"`
			CreatedAt                                        *int64          `json:"createdAt"`
			GrantModes                                       json.RawMessage `json:"grantModes"`
			BlocksInput                                      *bool           `json:"blocksInput"`
			Request                                          struct {
				Actor struct {
					ExternalID string `json:"externalId"`
				} `json:"actor"`
				ScopeVersion string `json:"scopeVersion"`
			} `json:"request"`
		}
		if json.Unmarshal(raw, &item) != nil || item.SessionID != sessionID || item.Command == "" {
			continue
		}
		blocks := item.BlocksInput == nil || *item.BlocksInput
		result = append(result, PendingApproval{ID: id, SessionID: item.SessionID, Command: item.Command, Reason: item.Reason, Matched: item.Matched, Purpose: item.Purpose, Summary: item.Summary, Kind: item.Kind, ActorID: item.Request.Actor.ExternalID, ScopeVersion: item.Request.ScopeVersion, ApprovalKey: item.ApprovalKey, CreatedAt: item.CreatedAt, GrantModes: item.GrantModes, Raw: append(json.RawMessage(nil), raw...), BlocksInput: blocks})
	}
	return result, rows.Err()
}

func (r *SessionRepository) Approval(ctx context.Context, id string) (*PendingApproval, error) {
	if exists, err := r.tableExists(ctx, "approvals"); err != nil {
		return nil, err
	} else if !exists {
		return nil, nil
	}
	var raw json.RawMessage
	err := r.pg.Pool.QueryRow(ctx, "SELECT json FROM approvals WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var item struct {
		SessionID                                        string `json:"sessionId"`
		Command, Reason, Matched, Purpose, Summary, Kind string
		ApprovalKey                                      string          `json:"approvalKey"`
		CreatedAt                                        *int64          `json:"createdAt"`
		GrantModes                                       json.RawMessage `json:"grantModes"`
		BlocksInput                                      *bool           `json:"blocksInput"`
		Request                                          struct {
			Actor struct {
				ExternalID string `json:"externalId"`
			} `json:"actor"`
			ScopeVersion string `json:"scopeVersion"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &item) != nil || item.SessionID == "" || item.Command == "" {
		return nil, nil
	}
	blocks := item.BlocksInput == nil || *item.BlocksInput
	return &PendingApproval{ID: id, SessionID: item.SessionID, Command: item.Command, Reason: item.Reason, Matched: item.Matched, Purpose: item.Purpose, Summary: item.Summary, Kind: item.Kind, ActorID: item.Request.Actor.ExternalID, ScopeVersion: item.Request.ScopeVersion, ApprovalKey: item.ApprovalKey, CreatedAt: item.CreatedAt, GrantModes: item.GrantModes, Raw: append(json.RawMessage(nil), raw...), BlocksInput: blocks}, nil
}

func (r *SessionRepository) ParticipantWindow(ctx context.Context, sessionID, principalID string) (*ParticipantWindow, error) {
	var window ParticipantWindow
	err := r.pg.Pool.QueryRow(ctx, "SELECT session_id,principal_id,valid_from,valid_to FROM participants WHERE session_id=$1 AND principal_id=$2", sessionID, principalID).Scan(&window.SessionID, &window.PrincipalID, &window.ValidFrom, &window.ValidTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &window, nil
}

func (r *SessionRepository) tableExists(ctx context.Context, table string) (bool, error) {
	var exists bool
	err := r.pg.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists)
	return exists, err
}

func (r *SessionRepository) ActiveParticipantIDs(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT principal_id FROM participants WHERE session_id=$1 AND valid_to IS NULL ORDER BY principal_id", sessionID)
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

func (r *SessionRepository) ParticipantWindows(ctx context.Context) ([]ParticipantWindow, error) {
	rows, err := r.pg.Pool.Query(ctx, "SELECT session_id,principal_id,valid_from,valid_to FROM participants")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ParticipantWindow{}
	for rows.Next() {
		var item ParticipantWindow
		if err := rows.Scan(&item.SessionID, &item.PrincipalID, &item.ValidFrom, &item.ValidTo); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SessionRepository) AttributedTurns(ctx context.Context) ([]AttributedTurn, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT p.principal_id,e.session_id,COUNT(*)::int,MIN(e.created_at),MAX(e.created_at) FROM participants p JOIN session_entries e ON e.session_id=p.session_id WHERE e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%') AND (((p.valid_from_seq IS NOT NULL AND e.seq>=p.valid_from_seq) OR (p.valid_from_seq IS NULL AND e.created_at>=p.valid_from)) AND ((p.valid_to_seq IS NOT NULL AND e.seq<p.valid_to_seq) OR (p.valid_to_seq IS NULL AND (p.valid_to IS NULL OR e.created_at<p.valid_to)))) GROUP BY p.principal_id,e.session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AttributedTurn{}
	for rows.Next() {
		var item AttributedTurn
		if err := rows.Scan(&item.PrincipalID, &item.SessionID, &item.Turns, &item.FirstAt, &item.LastAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SessionRepository) ListLLMRequests(ctx context.Context, sessionID string, turnSeq *int, orphans bool) ([]LLMRequest, error) {
	where := "session_id=$1"
	args := []any{sessionID}
	if orphans {
		where += " AND turn_seq IS NULL"
	} else if turnSeq != nil {
		args = append(args, *turnSeq)
		where += " AND turn_seq=$2"
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,session_id,turn_seq,step,model,scope_label,request,created_at,truncated,ttft_ms,duration_ms,step_gap_ms,tool_wall_json,usage_json,transport_json,gap_phases_json FROM session_llm_requests WHERE `+where+` ORDER BY created_at ASC,step ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LLMRequest{}
	for rows.Next() {
		var item LLMRequest
		if err := rows.Scan(&item.ID, &item.SessionID, &item.TurnSeq, &item.Step, &item.Model, &item.ScopeLabel, &item.Request, &item.CreatedAt, &item.Truncated, &item.TTFTMS, &item.DurationMS, &item.StepGapMS, &item.ToolWallMS, &item.Usage, &item.Transport, &item.GapPhases); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *SessionRepository) ListLLMRequestsForTurns(ctx context.Context, sessionID string, turns []int) ([]LLMRequest, error) {
	if len(turns) == 0 {
		return []LLMRequest{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT id,session_id,turn_seq,step,model,scope_label,request,created_at,truncated,ttft_ms,duration_ms,step_gap_ms,tool_wall_json,usage_json,transport_json,gap_phases_json FROM session_llm_requests WHERE session_id=$1 AND turn_seq=ANY($2) ORDER BY created_at ASC,step ASC`, sessionID, turns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LLMRequest{}
	for rows.Next() {
		var item LLMRequest
		if err := rows.Scan(&item.ID, &item.SessionID, &item.TurnSeq, &item.Step, &item.Model, &item.ScopeLabel, &item.Request, &item.CreatedAt, &item.Truncated, &item.TTFTMS, &item.DurationMS, &item.StepGapMS, &item.ToolWallMS, &item.Usage, &item.Transport, &item.GapPhases); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func sessionOrigin(threadRef string) string {
	if parts := strings.Split(threadRef, ":"); len(parts) == 4 && parts[0] == "agent" && parts[1] == "main" && (parts[2] == "cron" || parts[2] == "webhook" || parts[2] == "monitor") && parts[3] != "" {
		return parts[2]
	}
	if parts := strings.Split(threadRef, ":"); (len(parts) == 2 || len(parts) >= 3) && (parts[0] == "cron" || parts[0] == "webhook" || parts[0] == "monitor") && parts[1] != "" {
		return parts[0]
	}
	return "conversation"
}

func sessionCronID(threadRef string) string {
	if sessionOrigin(threadRef) != "cron" {
		return ""
	}
	parts := strings.Split(threadRef, ":")
	if len(parts) == 4 && parts[0] == "agent" {
		return parts[3]
	}
	return parts[1]
}

func sessionPreview(payload *string, limit int) string {
	if payload == nil {
		return ""
	}
	text := *payload
	var value any
	if json.Unmarshal([]byte(text), &value) != nil {
		return ""
	}
	if object, ok := value.(map[string]any); ok {
		message, ok := object["text"].(string)
		if !ok {
			return ""
		}
		text = message
	} else if scalar, ok := value.(string); ok {
		text = scalar
	} else {
		return ""
	}
	parts := []string{}
	for _, part := range strings.Split(text, "\n\n") {
		if !strings.HasPrefix(strings.TrimSpace(part), "[") {
			parts = append(parts, part)
		}
	}
	kept := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if kept == "" {
		kept = strings.TrimSpace(text)
	}
	kept = strings.Join(strings.Fields(kept), " ")
	if len([]rune(kept)) > limit {
		return string([]rune(kept)[:limit-1]) + "…"
	}
	return kept
}

func (r *SessionRepository) all(ctx context.Context, scope string, orgWide bool) ([]SessionSummary, error) {
	rows, err := r.pg.Pool.Query(ctx, `SELECT s.id,s.type,s.scope_id,s.thread_ref,COALESCE(s.turns,0),COALESCE(s.messages,0),COALESCE(s.last_activity,s.created_at),s.created_at,
 (SELECT payload FROM session_entries e WHERE e.session_id=s.id AND e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%') ORDER BY e.seq ASC LIMIT 1),
 (SELECT payload FROM session_entries e WHERE e.session_id=s.id AND e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%') ORDER BY e.seq DESC LIMIT 1)
 FROM sessions s WHERE $1::boolean OR s.scope_id=$2`, orgWide, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionSummary{}
	for rows.Next() {
		var item SessionSummary
		var first, last *string
		if err := rows.Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.Turns, &item.Messages, &item.LastActivity, &item.CreatedAt, &first, &last); err != nil {
			return nil, err
		}
		item.FirstMessage = sessionPreview(first, 160)
		item.LastMessage = sessionPreview(last, 100)
		result = append(result, item)
	}
	return result, rows.Err()
}

// SummariesForIDs returns the administrative summary projection for an
// explicit set of sessions. It is used for a person's detail view after their
// participant windows have established which session ids belong to them.
func (r *SessionRepository) SummariesForIDs(ctx context.Context, ids []string) ([]SessionSummary, error) {
	if len(ids) == 0 {
		return []SessionSummary{}, nil
	}
	rows, err := r.pg.Pool.Query(ctx, `SELECT s.id,s.type,s.scope_id,s.thread_ref,COALESCE(s.turns,0),COALESCE(s.messages,0),COALESCE(s.last_activity,s.created_at),s.created_at,
 (SELECT payload FROM session_entries e WHERE e.session_id=s.id AND e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%') ORDER BY e.seq ASC LIMIT 1),
 (SELECT payload FROM session_entries e WHERE e.session_id=s.id AND e.type='user' AND (e.payload IS NULL OR e.payload NOT LIKE '%"overheard":true%') ORDER BY e.seq DESC LIMIT 1)
 FROM sessions s WHERE s.id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionSummary{}
	for rows.Next() {
		var item SessionSummary
		var first, last *string
		if err := rows.Scan(&item.ID, &item.Type, &item.ScopeID, &item.ThreadRef, &item.Turns, &item.Messages, &item.LastActivity, &item.CreatedAt, &first, &last); err != nil {
			return nil, err
		}
		item.FirstMessage = sessionPreview(first, 160)
		item.LastMessage = sessionPreview(last, 100)
		result = append(result, item)
	}
	return result, rows.Err()
}

func sessionMatches(item SessionSummary, category, origin, cronID string) bool {
	actualOrigin := sessionOrigin(item.ThreadRef)
	background := actualOrigin != "conversation"
	if category == "background" && !background {
		return false
	}
	if category == "conversation" && background {
		return false
	}
	if origin == "cron" && actualOrigin != "cron" {
		return false
	}
	if origin == "other_background" && (!background || actualOrigin == "cron") {
		return false
	}
	return cronID == "" || sessionCronID(item.ThreadRef) == cronID
}

func (r *SessionRepository) Stats(ctx context.Context, scope string, orgWide bool, category, origin, cronID string) (SessionStats, error) {
	all, err := r.all(ctx, scope, orgWide)
	if err != nil {
		return SessionStats{}, err
	}
	result := SessionStats{ByType: map[string]int{}, ByTypeAll: map[string]int{}, TotalByCategory: map[string]int{"conversation": 0, "background": 0, "all": 0}}
	crons := map[string]bool{}
	for _, item := range all {
		actual := sessionOrigin(item.ThreadRef)
		bucket := item.Type
		if actual != "conversation" {
			bucket = actual
		}
		result.TotalByCategory["all"]++
		if actual == "conversation" {
			result.TotalByCategory["conversation"]++
		} else {
			result.TotalByCategory["background"]++
		}
		result.ByTypeAll[bucket]++
		if id := sessionCronID(item.ThreadRef); id != "" {
			crons[id] = true
		}
		if sessionMatches(item, category, origin, cronID) {
			result.Total++
			result.Turns += item.Turns
			result.ByType[bucket]++
		}
	}
	result.Crons = len(crons)
	return result, nil
}

func (r *SessionRepository) ListSummaries(ctx context.Context, scope string, orgWide bool, category, origin, cronID string, limit, offset int, beforeActivity int64, beforeID string) ([]SessionSummary, error) {
	all, err := r.all(ctx, scope, orgWide)
	if err != nil {
		return nil, err
	}
	filtered := make([]SessionSummary, 0, len(all))
	for _, item := range all {
		if sessionMatches(item, category, origin, cronID) && (beforeID == "" || item.LastActivity < beforeActivity || item.LastActivity == beforeActivity && item.ID < beforeID) {
			filtered = append(filtered, item)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].LastActivity != filtered[j].LastActivity {
			return filtered[i].LastActivity > filtered[j].LastActivity
		}
		return filtered[i].ID > filtered[j].ID
	})
	if beforeID == "" && offset > 0 {
		if offset >= len(filtered) {
			return []SessionSummary{}, nil
		}
		filtered = filtered[offset:]
	}
	if limit < len(filtered) {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (r *SessionRepository) CronGroups(ctx context.Context, scope string, orgWide bool) ([]map[string]any, error) {
	all, err := r.all(ctx, scope, orgWide)
	if err != nil {
		return nil, err
	}
	groups := map[string]map[string]any{}
	for _, item := range all {
		id := sessionCronID(item.ThreadRef)
		if id == "" {
			continue
		}
		group := groups[id]
		if group == nil {
			group = map[string]any{"cronId": id, "scopeId": item.ScopeID, "sessions": 0, "turns": 0, "messages": 0, "lastActivity": item.LastActivity, "createdAt": item.CreatedAt}
			groups[id] = group
		}
		group["sessions"] = group["sessions"].(int) + 1
		group["turns"] = group["turns"].(int) + item.Turns
		group["messages"] = group["messages"].(int) + item.Messages
		if item.LastActivity > group["lastActivity"].(int64) {
			group["lastActivity"] = item.LastActivity
		}
		if item.CreatedAt < group["createdAt"].(int64) {
			group["createdAt"] = item.CreatedAt
		}
	}
	result := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		result = append(result, group)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a["lastActivity"].(int64) != b["lastActivity"].(int64) {
			return a["lastActivity"].(int64) > b["lastActivity"].(int64)
		}
		return a["cronId"].(string) > b["cronId"].(string)
	})
	return result, nil
}
