package service

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	runnerv1 "github.com/loon-hejw/knowlega/api/qm/runner/v1"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestRunnerServiceOverGRPC(t *testing.T) {
	url := os.Getenv("QM_BACKEND_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QM_BACKEND_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pg, err := data.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('qm-backend-integration-tests'))"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('qm-backend-integration-tests'))")
	})
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE runs,run_signals,run_activity RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	runs := data.NewRunRepository(pg)
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	runnerv1.RegisterRunnerServiceServer(grpcServer, NewRunnerService(runs, time.Minute, 8))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := runnerv1.NewRunnerServiceClient(conn)
	enqueued, err := client.EnqueueRun(ctx, &runnerv1.EnqueueRunRequest{SessionId: "thread-1", Payload: []byte(`{"message":"work"}`), IdempotencyKey: "run-1", MaxAttempts: 3})
	if err != nil || enqueued.GetRunId() == "" || enqueued.GetDeduped() {
		t.Fatalf("enqueue=%#v err=%v", enqueued, err)
	}
	id := enqueued.GetRunId()
	duplicate, err := client.EnqueueRun(ctx, &runnerv1.EnqueueRunRequest{SessionId: "thread-1", Payload: []byte(`{"message":"work"}`), IdempotencyKey: "run-1", MaxAttempts: 3})
	if err != nil || duplicate.GetRunId() != id || !duplicate.GetDeduped() {
		t.Fatalf("duplicate enqueue=%#v err=%v", duplicate, err)
	}
	snapshot, err := client.GetRun(ctx, &runnerv1.GetRunRequest{RunId: id})
	if err != nil || !snapshot.GetFound() || snapshot.GetRun().GetStatus() != "pending" || string(snapshot.GetRun().GetPayload()) != `{"message":"work"}` {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	active, err := client.GetActiveRunForSession(ctx, &runnerv1.GetActiveRunForSessionRequest{SessionId: "thread-1"})
	if err != nil || !active.GetFound() || active.GetRun().GetRunId() != id {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	noActive, err := client.GetActiveRunForSession(ctx, &runnerv1.GetActiveRunForSessionRequest{SessionId: "thread-missing"})
	if err != nil || noActive.GetFound() {
		t.Fatalf("missing active=%#v err=%v", noActive, err)
	}
	activeIDs, err := client.ListActiveSessionIDs(ctx, &runnerv1.ListActiveSessionIDsRequest{})
	if err != nil || len(activeIDs.GetSessionIds()) != 1 || activeIDs.GetSessionIds()[0] != "thread-1" {
		t.Fatalf("active IDs=%#v err=%v", activeIDs, err)
	}
	missing, err := client.GetRun(ctx, &runnerv1.GetRunRequest{RunId: "missing"})
	if err != nil || missing.GetFound() {
		t.Fatalf("missing snapshot=%#v err=%v", missing, err)
	}
	if _, err := client.EnqueueRun(ctx, &runnerv1.EnqueueRunRequest{SessionId: "thread-invalid", Payload: []byte("not-json")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid enqueue error=%v, want InvalidArgument", err)
	}
	inline, err := client.ClaimRunByID(ctx, &runnerv1.ClaimRunByIDRequest{RunId: id, RunnerId: "inline"})
	if err != nil || inline.GetRunId() != id {
		t.Fatalf("inline claim=%#v err=%v", inline, err)
	}
	released, err := client.ReleaseRunLease(ctx, &runnerv1.ReleaseRunLeaseRequest{RunId: id, LeaseToken: inline.GetLeaseToken()})
	if err != nil || !released.GetAccepted() {
		t.Fatalf("inline release=%#v err=%v", released, err)
	}

	claim, err := client.ClaimRun(ctx, &runnerv1.ClaimRunRequest{RunnerId: "runner-a"})
	if err != nil {
		t.Fatal(err)
	}
	if claim.GetRunId() != id || claim.GetSessionId() != "thread-1" || claim.GetAttempt() != 2 || string(claim.GetPayload()) != `{"message":"work"}` {
		t.Fatalf("unexpected claim %#v", claim)
	}
	heartbeat, err := client.HeartbeatRun(ctx, &runnerv1.HeartbeatRunRequest{RunId: id, LeaseToken: claim.GetLeaseToken()})
	if err != nil || !heartbeat.GetAccepted() {
		t.Fatalf("heartbeat=%#v err=%v", heartbeat, err)
	}
	released, err = client.ReleaseRunLease(ctx, &runnerv1.ReleaseRunLeaseRequest{RunId: id, LeaseToken: claim.GetLeaseToken()})
	if err != nil || !released.GetAccepted() {
		t.Fatalf("release=%#v err=%v", released, err)
	}
	claim, err = client.ClaimRun(ctx, &runnerv1.ClaimRunRequest{RunnerId: "runner-a"})
	if err != nil || claim.GetRunId() != id {
		t.Fatalf("reclaim=%#v err=%v", claim, err)
	}
	failed, err := client.FailRun(ctx, &runnerv1.FailRunRequest{RunId: id, LeaseToken: claim.GetLeaseToken(), Reason: "temporary", Retry: true})
	if err != nil || !failed.GetAccepted() || !failed.GetRequeued() {
		t.Fatalf("fail=%#v err=%v", failed, err)
	}
	claim, err = client.ClaimRun(ctx, &runnerv1.ClaimRunRequest{RunnerId: "runner-a"})
	if err != nil || claim.GetRunId() != id {
		t.Fatalf("retry claim=%#v err=%v", claim, err)
	}
	appended, err := client.AppendRunEvent(ctx, &runnerv1.AppendRunEventRequest{RunId: id, LeaseToken: claim.GetLeaseToken(), Sequence: 1, IdempotencyKey: "event-1", Payload: []byte(`{"type":"text"}`)})
	if err != nil || !appended.GetAccepted() {
		t.Fatalf("append=%#v err=%v", appended, err)
	}
	completed, err := client.CompleteRun(ctx, &runnerv1.CompleteRunRequest{RunId: id, LeaseToken: claim.GetLeaseToken(), IdempotencyKey: "complete-1", Result: []byte(`{"status":"done"}`)})
	if err != nil || !completed.GetAccepted() {
		t.Fatalf("complete=%#v err=%v", completed, err)
	}
	signaled, err := client.SignalRun(ctx, &runnerv1.SignalRunRequest{RunId: id, Payload: []byte(`{"kind":"steer","text":"continue"}`)})
	if err != nil || !signaled.GetAccepted() {
		t.Fatalf("signal=%#v err=%v", signaled, err)
	}
	signals, err := client.TakePendingSignals(ctx, &runnerv1.TakePendingSignalsRequest{RunId: id})
	if err != nil || len(signals.GetSignals()) != 1 || signals.GetSignals()[0].GetKind() != "steer" {
		t.Fatalf("signals=%#v err=%v", signals, err)
	}
	activity, err := client.AppendRunActivity(ctx, &runnerv1.AppendRunActivityRequest{RunId: id, Activity: &runnerv1.RunActivity{Sequence: 2, Type: "text", Payload: []byte(`{"text":"hello"}`), CreatedAtUnixMs: time.Now().UnixMilli()}})
	if err != nil || !activity.GetAccepted() {
		t.Fatalf("activity=%#v err=%v", activity, err)
	}
	activities, err := client.ListRunActivity(ctx, &runnerv1.ListRunActivityRequest{RunId: id})
	if err != nil || len(activities.GetActivities()) != 2 || activities.GetActivities()[1].GetType() != "text" {
		t.Fatalf("activities=%#v err=%v", activities, err)
	}
	id, _, err = runs.Enqueue(ctx, "thread-2", []byte(`{"message":"reap"}`), "run-2", 3)
	if err != nil {
		t.Fatal(err)
	}
	claim, err = client.ClaimRun(ctx, &runnerv1.ClaimRunRequest{RunnerId: "runner-a"})
	if err != nil || claim.GetRunId() != id {
		t.Fatalf("reap claim=%#v err=%v", claim, err)
	}
	if _, err := pg.Pool.Exec(ctx, "UPDATE runs SET lease_expires_at=$1 WHERE id=$2", time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	reaped, err := client.ReapExpiredRuns(ctx, &runnerv1.ReapExpiredRunsRequest{})
	if err != nil || reaped.GetRequeued() != 1 || len(reaped.GetEvents()) != 1 {
		t.Fatalf("reaped=%#v err=%v", reaped, err)
	}
}
