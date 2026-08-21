package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func openIntegrationPostgres(t *testing.T) *Postgres {
	t.Helper()
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Pool.Exec(context.Background(), "SELECT pg_advisory_lock(hashtext('qm-backend-integration-tests'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('qm-backend-integration-tests'))")
	})
	if err := pg.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = pg.Pool.Exec(context.Background(), `TRUNCATE runtime_task_events,runtime_tasks,agent_harness_state,context_request_tokens,context_requests,session_tape,knowledge_scopes,projects,durable_map_versions,deactivated_principals,source_auth_replay,runs,run_signals,run_activity,
directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,
acl_grants,admin_grants,audit_log RESTART IDENTITY`)
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

func TestKnowledgeScopeEnsureRefreshesMovedRoot(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	repository := NewKnowledgeScopeRepository(pg)
	initial := KnowledgeScope{
		OrgID: "acme", ExternalScopeID: "group:web-project-1", Kind: "project",
		ProjectID: "knowledge-project-1", ProjectName: "Old name", RootPath: "/tmp/lease/knowledge", Status: "queued",
	}
	if _, err := repository.Ensure(ctx, initial); err != nil {
		t.Fatal(err)
	}
	initial.ProjectName = "Current name"
	initial.RootPath = "/var/lib/qm/knowledge"
	initial.Status = "ready"
	updated, err := repository.Ensure(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProjectName != initial.ProjectName || updated.RootPath != initial.RootPath || updated.Status != "ready" {
		t.Fatalf("updated scope=%+v", updated)
	}
}

func TestHarnessSessionRepositoryPersistsAdapterSwitches(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	repository := NewHarnessSessionRepository(pg)
	previous, changed, err := repository.Select(ctx, "session-1", "pi")
	if err != nil || previous != "" || changed {
		t.Fatalf("first previous=%q changed=%v err=%v", previous, changed, err)
	}
	previous, changed, err = repository.Select(ctx, "session-1", "pi")
	if err != nil || previous != "pi" || changed {
		t.Fatalf("same previous=%q changed=%v err=%v", previous, changed, err)
	}
	previous, changed, err = repository.Select(ctx, "session-1", "codex")
	if err != nil || previous != "pi" || !changed {
		t.Fatalf("switch previous=%q changed=%v err=%v", previous, changed, err)
	}
	if err := repository.Delete(ctx, "session-1"); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM agent_harness_state WHERE session_id='session-1'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}

func TestSessionRepositoryRejectsThreadScopeMismatchWithoutMutation(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	const threadRef = "web:alice:scope-mismatch"
	_, _ = pg.Pool.Exec(ctx, "DELETE FROM sessions WHERE thread_ref=$1", threadRef)
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM sessions WHERE thread_ref=$1", threadRef)
	})
	repository := NewSessionRepository(pg)
	created, err := repository.GetOrCreateByThread(ctx, threadRef, "group", "group:web-project-one", "Project one", "web")
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.GetOrCreateByThread(ctx, threadRef, "group", "group:web-project-two", "Project two", "web")
	var mismatch *SessionScopeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err=%v", err)
	}
	stored, err := repository.GetByThread(ctx, threadRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.ID != created.ID || stored.ScopeID != "group:web-project-one" || stored.Type != "group" {
		t.Fatalf("stored=%+v created=%+v", stored, created)
	}
}

func TestSessionRepositoryAppendsEntriesAndLLMRequestsLikeNode(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	const sessionID = "go-agent-session-persistence"
	_, _ = pg.Pool.Exec(ctx, "DELETE FROM session_llm_requests WHERE session_id=$1", sessionID)
	_, _ = pg.Pool.Exec(ctx, "DELETE FROM session_tape WHERE session_id=$1", sessionID)
	_, _ = pg.Pool.Exec(ctx, "DELETE FROM session_entries WHERE session_id=$1", sessionID)
	_, _ = pg.Pool.Exec(ctx, "DELETE FROM sessions WHERE id=$1", sessionID)
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM session_llm_requests WHERE session_id=$1", sessionID)
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM session_tape WHERE session_id=$1", sessionID)
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM session_entries WHERE session_id=$1", sessionID)
		_, _ = pg.Pool.Exec(context.Background(), "DELETE FROM sessions WHERE id=$1", sessionID)
	})
	if _, err := pg.Pool.Exec(ctx, `INSERT INTO sessions(id,type,scope_id,thread_ref,created_at) VALUES($1,'dm','personal:alice',$2,$3)`, sessionID, "go-agent-thread-persistence", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	repository := NewSessionRepository(pg)
	entries := []NewSessionEntry{
		{Type: "user", ScopeLabel: "personal:alice", Payload: json.RawMessage(`{"text":"first"}`)},
		{Type: "user", ScopeLabel: "personal:alice", Payload: json.RawMessage(`{"text":"ambient","overheard":true}`)},
		{Type: "assistant", ScopeLabel: "personal:alice", Payload: json.RawMessage(`{"text":"reply"}`)},
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, len(entries))
	for _, entry := range entries {
		entry := entry
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.AppendEntry(ctx, sessionID, entry)
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, err := repository.Entries(ctx, sessionID, 0, 0)
	if err != nil || len(stored) != 3 {
		t.Fatalf("entries=%#v err=%v", stored, err)
	}
	for index, entry := range stored {
		if entry.Sequence != index || index == 0 && entry.ParentSequence != nil || index > 0 && (entry.ParentSequence == nil || *entry.ParentSequence != index-1) {
			t.Fatalf("entry[%d]=%#v", index, entry)
		}
	}
	var messages, turns int
	if err := pg.Pool.QueryRow(ctx, "SELECT messages,turns FROM sessions WHERE id=$1", sessionID).Scan(&messages, &turns); err != nil || messages != 3 || turns != 1 {
		t.Fatalf("messages=%d turns=%d err=%v", messages, turns, err)
	}
	turnSeq := 0
	request, err := repository.RecordLLMRequest(ctx, sessionID, NewLLMRequest{
		TurnSeq: &turnSeq, Step: 1, Model: "model-pi", ScopeLabel: "personal:alice",
		Request: json.RawMessage(`{"messages":[{"role":"user","content":"first"}]}`),
		Usage:   json.RawMessage(`{"input":10,"output":2}`),
	})
	if err != nil || request.ID == "" || request.TurnSeq == nil || *request.TurnSeq != 0 {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	requests, err := repository.ListLLMRequests(ctx, sessionID, &turnSeq, false)
	if err != nil || len(requests) != 1 || requests[0].Model != "model-pi" || string(requests[0].Usage) != `{"input":10,"output":2}` {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
	wait = sync.WaitGroup{}
	errorsFound = make(chan error, 3)
	for index := 0; index < 3; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := repository.AppendTape(ctx, sessionID, NewTapeRecord{Kind: "message", Harness: "pi", ScopeLabel: "personal:alice", Payload: json.RawMessage(`{"role":"assistant"}`), EntrySequence: &index})
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	tape, err := repository.Tape(ctx, sessionID, -1)
	if err != nil || len(tape) != 3 {
		t.Fatalf("tape=%#v err=%v", tape, err)
	}
	for index, record := range tape {
		if record.Sequence != index || record.Harness != "pi" {
			t.Fatalf("tape[%d]=%#v", index, record)
		}
	}
	coverage, err := repository.TapeCoverage(ctx, sessionID)
	if err != nil || coverage != -1 {
		t.Fatalf("coverage=%d err=%v", coverage, err)
	}
	coveredSeq := 2
	if _, err := repository.AppendTape(ctx, sessionID, NewTapeRecord{Kind: "annotation", ScopeLabel: "personal:alice", Payload: json.RawMessage(`{"turnEnd":true}`), EntrySequence: &coveredSeq}); err != nil {
		t.Fatal(err)
	}
	coverage, err = repository.TapeCoverage(ctx, sessionID)
	if err != nil || coverage != coveredSeq {
		t.Fatalf("coverage=%d err=%v", coverage, err)
	}
}

func TestContextRequestRepositoryLifecycle(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	repository := NewContextRequestRepository(pg)
	request, err := repository.Create(ctx, "slack", []byte(`{"channelId":"C123456","count":10}`), "encrypted-viewer-token")
	if err != nil || request == nil || request.Status != "pending" {
		t.Fatalf("create=%#v err=%v", request, err)
	}
	pending, err := repository.Pending(ctx, "slack", time.Now())
	if err != nil || len(pending) != 1 || pending[0].ID != request.ID || pending[0].ViewerTokenEncrypted != "encrypted-viewer-token" {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	result := []byte(`{"messages":[{"text":"hello"}],"hasMore":false}`)
	ok, err := repository.Fulfill(ctx, request.ID, result, nil)
	if err != nil || !ok {
		t.Fatalf("fulfill=%v err=%v", ok, err)
	}
	stored, err := repository.Get(ctx, request.ID)
	if err != nil || stored == nil || stored.Status != "done" || len(stored.Result) == 0 {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	var tokens int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM context_request_tokens WHERE request_id=$1", request.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens=%d err=%v", tokens, err)
	}
	deleted, err := repository.Delete(ctx, request.ID)
	if err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
}

func TestContextRequestRepositoryExpiresPendingRows(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	repository := NewContextRequestRepository(pg)
	request, err := repository.Create(ctx, "slack", []byte(`{"count":1}`), "")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repository.Pending(ctx, "slack", time.UnixMilli(request.CreatedAt).Add(ContextRequestExpiry+time.Millisecond))
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	stored, err := repository.Get(ctx, request.ID)
	if err != nil || stored != nil {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestRuntimeTaskRepositoryLifecycle(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	tasks := NewRuntimeTaskRepository(pg)
	id, deduped, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{
		Kind:           "deploy.apply",
		Payload:        []byte(`{"deploymentId":"d1"}`),
		IdempotencyKey: "deploy-d1-v1",
		Priority:       3,
		MaxAttempts:    2,
	})
	if err != nil || deduped || id == "" {
		t.Fatalf("enqueue id=%q deduped=%v err=%v", id, deduped, err)
	}
	duplicateID, deduped, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{
		Kind:           "deploy.apply",
		Payload:        []byte(`{"deploymentId":"d1"}`),
		IdempotencyKey: "deploy-d1-v1",
	})
	if err != nil || !deduped || duplicateID != id {
		t.Fatalf("dedupe id=%q deduped=%v err=%v", duplicateID, deduped, err)
	}
	claim, err := tasks.Claim(ctx, "worker-a", []string{"oauth.refresh"}, time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("wrong-kind claim=%#v err=%v", claim, err)
	}
	claim, err = tasks.Claim(ctx, "worker-a", []string{"deploy.apply"}, time.Minute)
	if err != nil || claim == nil || claim.ID != id || claim.Attempts != 1 || claim.ClaimedLeaseToken == "" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	alive, cancelled, err := tasks.Heartbeat(ctx, id, claim.ClaimedLeaseToken, time.Minute)
	if err != nil || !alive || cancelled {
		t.Fatalf("heartbeat alive=%v cancelled=%v err=%v", alive, cancelled, err)
	}
	accepted, err := tasks.AppendEvent(ctx, id, claim.ClaimedLeaseToken, RuntimeTaskEvent{Sequence: 1, Type: "progress", Payload: []byte(`{"step":"build"}`)})
	if err != nil || !accepted {
		t.Fatalf("append event accepted=%v err=%v", accepted, err)
	}
	accepted, err = tasks.AppendEvent(ctx, id, claim.ClaimedLeaseToken, RuntimeTaskEvent{Sequence: 1, Type: "progress", Payload: []byte(`{"step":"build"}`)})
	if err != nil || !accepted {
		t.Fatalf("idempotent event accepted=%v err=%v", accepted, err)
	}
	events, err := tasks.ListEvents(ctx, id)
	if err != nil || len(events) != 1 || events[0].Type != "progress" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	events, err = tasks.ListEventsAfter(ctx, id, 1)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after cursor=%#v err=%v", events, err)
	}
	accepted, requeued, err := tasks.Fail(ctx, id, claim.ClaimedLeaseToken, "temporary", true, 0)
	if err != nil || !accepted || !requeued {
		t.Fatalf("fail accepted=%v requeued=%v err=%v", accepted, requeued, err)
	}
	claim, err = tasks.Claim(ctx, "worker-b", []string{"deploy.apply"}, time.Minute)
	if err != nil || claim == nil || claim.Attempts != 2 {
		t.Fatalf("reclaim=%#v err=%v", claim, err)
	}
	completed, err := tasks.Complete(ctx, id, claim.ClaimedLeaseToken, []byte(`{"status":"ready"}`))
	if err != nil || !completed {
		t.Fatalf("complete=%v err=%v", completed, err)
	}
	stored, err := tasks.Get(ctx, id)
	if err != nil || stored == nil || stored.Status != "succeeded" || string(stored.Result) != `{"status": "ready"}` {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestRuntimeTaskRepositoryCancellationAndReap(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	tasks := NewRuntimeTaskRepository(pg)
	runningID, _, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{Kind: "sandbox.exec", Payload: []byte(`{}`), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := tasks.Claim(ctx, "worker-a", []string{"sandbox.exec"}, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	cancelled, err := tasks.Cancel(ctx, runningID)
	if err != nil || !cancelled {
		t.Fatalf("cancel=%v err=%v", cancelled, err)
	}
	alive, requested, err := tasks.Heartbeat(ctx, runningID, claim.ClaimedLeaseToken, time.Minute)
	if err != nil || !alive || !requested {
		t.Fatalf("heartbeat alive=%v requested=%v err=%v", alive, requested, err)
	}
	acknowledged, err := tasks.AcknowledgeCancellation(ctx, runningID, claim.ClaimedLeaseToken)
	if err != nil || !acknowledged {
		t.Fatalf("acknowledge=%v err=%v", acknowledged, err)
	}

	expiredID, _, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{Kind: "sandbox.exec", Payload: []byte(`{}`), MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := tasks.Claim(ctx, "worker-b", []string{"sandbox.exec"}, time.Second)
	if err != nil || expired == nil || expired.ID != expiredID {
		t.Fatalf("expired claim=%#v err=%v", expired, err)
	}
	if _, err := pg.Pool.Exec(ctx, "UPDATE runtime_tasks SET lease_expires_at=$1 WHERE id=$2", time.Now().Add(-time.Second).UnixMilli(), expiredID); err != nil {
		t.Fatal(err)
	}
	requeued, dead, err := tasks.ReapExpired(ctx)
	if err != nil || requeued != 1 || dead != 0 {
		t.Fatalf("reap requeued=%d dead=%d err=%v", requeued, dead, err)
	}
	reclaimed, err := tasks.Claim(ctx, "worker-c", []string{"sandbox.exec"}, time.Second)
	if err != nil || reclaimed == nil || reclaimed.Attempts != 2 {
		t.Fatalf("reclaim=%#v err=%v", reclaimed, err)
	}
	if _, err := pg.Pool.Exec(ctx, "UPDATE runtime_tasks SET lease_expires_at=$1 WHERE id=$2", time.Now().Add(-time.Second).UnixMilli(), expiredID); err != nil {
		t.Fatal(err)
	}
	requeued, dead, err = tasks.ReapExpired(ctx)
	if err != nil || requeued != 0 || dead != 1 {
		t.Fatalf("terminal reap requeued=%d dead=%d err=%v", requeued, dead, err)
	}
}

func TestRuntimeTaskRepositorySerializesSessionTurns(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	tasks := NewRuntimeTaskRepository(pg)
	firstID, _, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{Kind: "agent.turn", Payload: []byte(`{"turn":1}`), SerialKey: "session:s1", Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{Kind: "agent.turn", Payload: []byte(`{"turn":2}`), SerialKey: "session:s1", Priority: 9})
	if err != nil {
		t.Fatal(err)
	}
	otherID, _, err := tasks.Enqueue(ctx, EnqueueRuntimeTaskInput{Kind: "agent.turn", Payload: []byte(`{"turn":3}`), SerialKey: "session:s2", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := tasks.Claim(ctx, "worker-a", []string{"agent.turn"}, time.Minute)
	if err != nil || first == nil || first.ID != firstID || first.SerialKey == nil || *first.SerialKey != "session:s1" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	other, err := tasks.Claim(ctx, "worker-b", []string{"agent.turn"}, time.Minute)
	if err != nil || other == nil || other.ID != otherID {
		t.Fatalf("other=%#v err=%v", other, err)
	}
	if ok, err := tasks.Complete(ctx, first.ID, first.ClaimedLeaseToken, []byte(`{}`)); err != nil || !ok {
		t.Fatalf("complete first=%v err=%v", ok, err)
	}
	second, err := tasks.Claim(ctx, "worker-c", []string{"agent.turn"}, time.Minute)
	if err != nil || second == nil || second.ID != secondID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestKeychainConnectorDeletionLazilyCreatesDurableMaps(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	// These are Node-owned DurableMap tables, intentionally absent from the Go
	// migrations. A first Go-owned OAuth disconnect must create the same maps
	// lazily rather than fail until a Node worker happens to touch keychain.
	if _, err := pg.Pool.Exec(ctx, "DROP TABLE IF EXISTS keychain_credentials,keychain_grants,keychain_asks"); err != nil {
		t.Fatal(err)
	}
	if err := NewKeychainStatusRepository(pg).DeleteConnectorTokens(ctx, "U1", []string{"slack.com"}); err != nil {
		t.Fatal(err)
	}
	var credentialsExist, grantsExist bool
	if err := pg.Pool.QueryRow(ctx, "SELECT to_regclass('keychain_credentials') IS NOT NULL,to_regclass('keychain_grants') IS NOT NULL").Scan(&credentialsExist, &grantsExist); err != nil {
		t.Fatal(err)
	}
	var credentialVersion int64
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='keychain_credentials'").Scan(&credentialVersion); err != nil {
		t.Fatal(err)
	}
	if !credentialsExist || !grantsExist || credentialVersion != 3 {
		t.Fatalf("lazy keychain maps credentials=%v grants=%v credential-version=%d", credentialsExist, grantsExist, credentialVersion)
	}
	if _, err := pg.Pool.Exec(ctx, "INSERT INTO keychain_credentials(id,json) VALUES($1,$2::jsonb)", "ordinary-credential", `{"id":"ordinary-credential","ownerId":"U1","service":"github","kind":"env","secretEnc":"not-returned"}`); err != nil {
		t.Fatal(err)
	}
	status, err := NewKeychainStatusRepository(pg).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Credentials) != 1 || len(status.Grants) != 0 || len(status.Asks) != 0 || status.Credentials[0]["id"] != "ordinary-credential" {
		t.Fatalf("partially initialized keychain status=%#v", status)
	}
}

func TestRunRepositoryLeaseEventAndSignal(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	runs := NewRunRepository(pg)
	id, deduped, err := runs.Enqueue(ctx, "session-1", []byte(`{"message":"work"}`), "turn-1", 3)
	if err != nil || deduped {
		t.Fatalf("enqueue id=%q deduped=%v err=%v", id, deduped, err)
	}
	duplicateID, deduped, err := runs.Enqueue(ctx, "session-1", []byte(`{"message":"work"}`), "turn-1", 3)
	if err != nil || !deduped || duplicateID != id {
		t.Fatalf("dedup enqueue id=%q deduped=%v err=%v", duplicateID, deduped, err)
	}
	claim, err := runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil || claim.ID != id || claim.Attempt != 1 {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	alive, err := runs.Heartbeat(ctx, claim.ID, claim.LeaseToken, time.Minute)
	if err != nil || !alive {
		t.Fatalf("heartbeat alive=%v err=%v", alive, err)
	}
	accepted, err := runs.AppendEvent(ctx, claim.ID, claim.LeaseToken, 1, "event-1", []byte(`{"type":"text","text":"started"}`))
	if err != nil || !accepted {
		t.Fatalf("append event accepted=%v err=%v", accepted, err)
	}
	accepted, err = runs.AppendEvent(ctx, claim.ID, claim.LeaseToken, 1, "event-1", []byte(`{"type":"text","text":"started"}`))
	if err != nil || !accepted {
		t.Fatalf("idempotent append accepted=%v err=%v", accepted, err)
	}
	var eventCount int
	if err := pg.Pool.QueryRow(ctx, "SELECT count(*) FROM run_activity WHERE run_id=$1", claim.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("expected one event, got %d", eventCount)
	}
	completed, err := runs.Complete(ctx, claim.ID, claim.LeaseToken, "complete-1", []byte(`{"status":"done"}`))
	if err != nil || !completed {
		t.Fatalf("complete completed=%v err=%v", completed, err)
	}
	completed, err = runs.Complete(ctx, claim.ID, claim.LeaseToken, "complete-1", []byte(`{"status":"done"}`))
	if err != nil || !completed {
		t.Fatalf("idempotent complete completed=%v err=%v", completed, err)
	}
	signaled, err := runs.Signal(ctx, claim.ID, []byte(`{"kind":"steer","text":"continue"}`))
	if err != nil || !signaled {
		t.Fatalf("signal signaled=%v err=%v", signaled, err)
	}
	signals, err := runs.TakePendingSignals(ctx, claim.ID)
	if err != nil || len(signals) != 1 || signals[0].Kind != "steer" {
		t.Fatalf("signals=%#v err=%v", signals, err)
	}
	accepted, err = runs.AppendActivity(ctx, claim.ID, RunActivity{Sequence: 2, Type: "text", Payload: []byte(`{"text":"hello"}`), CreatedAt: time.Now().UnixMilli()})
	if err != nil || !accepted {
		t.Fatalf("append activity accepted=%v err=%v", accepted, err)
	}
	activities, err := runs.ListActivity(ctx, claim.ID)
	if err != nil || len(activities) != 2 || activities[1].Type != "text" {
		t.Fatalf("activities=%#v err=%v", activities, err)
	}
}

func TestRunRepositoryReleaseAndFail(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	runs := NewRunRepository(pg)
	id, _, err := runs.Enqueue(ctx, "session-1", []byte(`{"message":"work"}`), "turn-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	released, err := runs.ReleaseLease(ctx, id, claim.LeaseToken)
	if err != nil || !released {
		t.Fatalf("released=%v err=%v", released, err)
	}
	claim, err = runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil || claim.Attempt != 2 {
		t.Fatalf("reclaim=%#v err=%v", claim, err)
	}
	accepted, requeued, err := runs.Fail(ctx, id, claim.LeaseToken, "temporary", true)
	if err != nil || !accepted || !requeued {
		t.Fatalf("retry failure accepted=%v requeued=%v err=%v", accepted, requeued, err)
	}
	claim, err = runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil || claim.Attempt != 3 {
		t.Fatalf("retry claim=%#v err=%v", claim, err)
	}
	accepted, requeued, err = runs.Fail(ctx, id, claim.LeaseToken, "terminal", true)
	if err != nil || !accepted || requeued {
		t.Fatalf("terminal failure accepted=%v requeued=%v err=%v", accepted, requeued, err)
	}
	var state string
	if err := pg.Pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("expected failed state, got %q", state)
	}
}

func TestRunRepositoryReapExpired(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	runs := NewRunRepository(pg)
	id, _, err := runs.Enqueue(ctx, "session-1", []byte(`{"message":"work"}`), "turn-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if _, err := pg.Pool.Exec(ctx, "UPDATE runs SET lease_expires_at=$1 WHERE id=$2", time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	events, err := runs.ReapExpired(ctx, 0, 8)
	if err != nil || len(events) != 1 || events[0].Outcome != "requeued" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	claim, err = runs.Claim(ctx, "runner-a", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("reclaim=%#v err=%v", claim, err)
	}
	if _, err := pg.Pool.Exec(ctx, "UPDATE runs SET lease_expires_at=$1,started_at=$2 WHERE id=$3", time.Now().Add(-time.Second).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	events, err = runs.ReapExpired(ctx, time.Minute, 8)
	if err != nil || len(events) != 1 || events[0].Outcome != "parked" {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	var state string
	if err := pg.Pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("expected failed state, got %q", state)
	}
}

func TestCronRepositoryClaimsDueSlotOnce(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE crons"); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(-time.Minute).UnixMilli()
	crons := NewCronRepository(pg)
	record, err := crons.Create(ctx, []byte(`{"owner":"U1","schedule":{"everyMs":60000}}`), &next)
	if err != nil {
		t.Fatal(err)
	}
	due, err := crons.ListDue(ctx, time.Now().UnixMilli(), 10)
	if err != nil || len(due) != 1 || due[0].ID != record.ID {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	after := time.Now().Add(time.Minute).UnixMilli()
	claimed, err := crons.ClaimSlot(ctx, record.ID, next, time.Now().UnixMilli(), &after)
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	claimed, err = crons.ClaimSlot(ctx, record.ID, next, time.Now().UnixMilli(), &after)
	if err != nil || claimed {
		t.Fatalf("duplicate claim=%v err=%v", claimed, err)
	}
}

func TestProjectRepositoryCompatibility(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	repo := NewProjectRepository(pg, "acme")
	directory := NewDirectoryRepository(pg, "acme")
	members := []DirectoryMember{{PrincipalID: "owner", DisplayName: "Owner", Type: "internal"}, {PrincipalID: "member", DisplayName: "Member", Type: "internal"}, {PrincipalID: "guest", DisplayName: "Guest", Type: "guest"}}
	if err := directory.Sync(ctx, DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	project, err := repo.Create(ctx, "owner", "  Release   plan ")
	if err != nil {
		t.Fatal(err)
	}
	if project == nil || project.Name != "Release plan" || len(project.MemberIDs) != 1 {
		t.Fatalf("unexpected project %#v", project)
	}
	result, err := repo.AddMember(ctx, project.ID, "owner", "member")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || len(result.Project.MemberIDs) != 2 {
		t.Fatalf("unexpected add result %#v", result)
	}
	result, err = repo.AddMember(ctx, project.ID, "owner", "guest")
	if err != nil || result.Status != "invalid_member" {
		t.Fatalf("guest project member result=%#v err=%v", result, err)
	}
	visible, err := repo.ListForMember(ctx, "member")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != project.ID {
		t.Fatalf("unexpected projects %#v", visible)
	}
	if err := (&DirectoryRepository{pg: pg, orgID: "acme"}).SetActive(ctx, "member", false); err != nil {
		t.Fatal(err)
	}
	visible, err = repo.ListForMember(ctx, "member")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 0 {
		t.Fatalf("deactivated member retained project visibility %#v", visible)
	}
}

func TestDirectoryAndACLRepositories(t *testing.T) {
	pg := openIntegrationPostgres(t)
	ctx := context.Background()
	directory := NewDirectoryRepository(pg, "acme")
	members := []DirectoryMember{{PrincipalID: "U1", DisplayName: "Alice Smith", Type: "internal", SlackID: "U11111111"}, {PrincipalID: "U2", DisplayName: "Alice Jones", Type: "internal"}, {PrincipalID: "G1", DisplayName: "Guest", Type: "guest"}}
	if err := directory.Sync(ctx, DirectoryUpdate{Members: &members}); err != nil {
		t.Fatal(err)
	}
	matches, err := directory.Resolve(ctx, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected internal directory matches, got %#v", matches)
	}
	acl := NewACLRepository(pg)
	expected := []Grant{}
	replacement := []Grant{{OwnerScopeID: "group:web-project-p1", Path: "skills/x", GranteeScopeID: "personal:U1", Permission: "read", GrantedBy: "U1"}}
	ok, err := acl.ReplaceResource(ctx, "group:web-project-p1", "skills/x", expected, replacement)
	if err != nil || !ok {
		t.Fatalf("replace ACL: ok=%v err=%v", ok, err)
	}
	grants, err := acl.List(ctx, "group:web-project-p1", "skills/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0] != replacement[0] {
		t.Fatalf("unexpected grants %#v", grants)
	}
	if err := acl.PutAdminGrant(ctx, AdminGrant{PrincipalID: "U1", ScopeID: "org:acme", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	admins, err := acl.ListAdminGrants(ctx, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 1 || admins[0].Role != "owner" {
		t.Fatalf("unexpected admin grants %#v", admins)
	}
}
