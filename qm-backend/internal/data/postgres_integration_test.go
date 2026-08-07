package data

import (
	"context"
	"os"
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
	_, err = pg.Pool.Exec(context.Background(), `TRUNCATE projects,durable_map_versions,deactivated_principals,source_auth_replay,runs,run_signals,run_activity,
directory_members,directory_channels,directory_channel_members,directory_group_members,directory_sync,directory_meta,
acl_grants,admin_grants,audit_log RESTART IDENTITY`)
	if err != nil {
		t.Fatal(err)
	}
	return pg
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
