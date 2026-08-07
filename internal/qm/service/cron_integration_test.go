package service

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	cronv1 "github.com/loon-hejw/knowlega/api/qm/cron/v1"
	"github.com/loon-hejw/knowlega/internal/qm/data"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestCronServicePersistsSchedulerStateOverGRPC(t *testing.T) {
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE crons"); err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	cronv1.RegisterCronServiceServer(grpcServer, NewCronService(data.NewCronRepository(pg)))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := cronv1.NewCronServiceClient(connection)

	now := time.Now().UnixMilli()
	initial := map[string]any{
		"id":         "cron-1",
		"owner":      "U1",
		"enabled":    true,
		"createdAt":  now - 60_000,
		"nextFireAt": now - 1_000,
		"schedule":   map[string]any{"everyMs": 60_000, "firstFireAt": now - 60_000},
		"fireLog":    []any{},
	}
	payload, _ := json.Marshal(initial)
	put, err := client.PutIfAbsent(ctx, &cronv1.PutIfAbsentRequest{Id: "cron-1", Payload: payload})
	if err != nil || put.GetCron().GetId() != "cron-1" {
		t.Fatalf("put=%#v err=%v", put, err)
	}
	due, err := client.ListDue(ctx, &cronv1.ListDueRequest{NowUnixMs: now, Limit: 10})
	if err != nil || len(due.GetCrons()) != 1 || due.GetCrons()[0].GetId() != "cron-1" {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	next := now + 60_000
	claim, err := client.ClaimSlot(ctx, &cronv1.ClaimSlotRequest{Id: "cron-1", ScheduledAtUnixMs: now - 1_000, FiredAtUnixMs: now, NextFireAtUnixMs: next, HasNextFireAt: true})
	if err != nil || !claim.GetClaimed() {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	claim, err = client.ClaimSlot(ctx, &cronv1.ClaimSlotRequest{Id: "cron-1", ScheduledAtUnixMs: now - 1_000, FiredAtUnixMs: now, NextFireAtUnixMs: next, HasNextFireAt: true})
	if err != nil || claim.GetClaimed() {
		t.Fatalf("duplicate claim=%#v err=%v", claim, err)
	}
	entry := []byte(`{"fireKey":"cron:cron-1:slot","threadRef":"cron:cron-1:fire:slot","firedAt":1,"status":"completed"}`)
	recorded, err := client.RecordFire(ctx, &cronv1.RecordFireRequest{Id: "cron-1", Entry: entry})
	if err != nil || !recorded.GetUpdated() {
		t.Fatalf("record=%#v err=%v", recorded, err)
	}
	merged, err := client.Merge(ctx, &cronv1.MergeRequest{Id: "cron-1", Patch: []byte(`{"title":"Daily work"}`)})
	if err != nil || !merged.GetFound() {
		t.Fatalf("merge=%#v err=%v", merged, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(merged.GetCron().GetPayload(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["nextFireAt"] != float64(next) || stored["lastFiredAt"] != float64(now) || stored["title"] != "Daily work" || len(stored["fireLog"].([]any)) != 1 {
		t.Fatalf("cron JSON diverged from scheduling state: %#v", stored)
	}
	rollback, err := client.UnclaimSlot(ctx, &cronv1.UnclaimSlotRequest{Id: "cron-1", ScheduledAtUnixMs: now - 1_000, FiredAtUnixMs: now})
	if err != nil || !rollback.GetRestored() {
		t.Fatalf("rollback=%#v err=%v", rollback, err)
	}
	get, err := client.Get(ctx, &cronv1.GetRequest{Id: "cron-1"})
	if err != nil || !get.GetFound() {
		t.Fatalf("get=%#v err=%v", get, err)
	}
	stored = nil
	if err := json.Unmarshal(get.GetCron().GetPayload(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["nextFireAt"] != float64(now-1_000) {
		t.Fatalf("unclaim did not restore next fire: %#v", stored)
	}
	if _, ok := stored["lastFiredAt"]; ok {
		t.Fatalf("unclaim did not restore an empty last fire time: %#v", stored)
	}
	due, err = client.ListDue(ctx, &cronv1.ListDueRequest{NowUnixMs: now, Limit: 10})
	if err != nil || len(due.GetCrons()) != 1 {
		t.Fatalf("restored due=%#v err=%v", due, err)
	}
	marked, err := client.MarkFired(ctx, &cronv1.MarkFiredRequest{Id: "cron-1", FiredAtUnixMs: now, NextFireAtUnixMs: next, HasNextFireAt: true})
	if err != nil || !marked.GetUpdated() {
		t.Fatalf("mark=%#v err=%v", marked, err)
	}
	taken, err := client.Take(ctx, &cronv1.TakeRequest{Id: "cron-1"})
	if err != nil || !taken.GetFound() || taken.GetCron().GetId() != "cron-1" {
		t.Fatalf("take=%#v err=%v", taken, err)
	}
	get, err = client.Get(ctx, &cronv1.GetRequest{Id: "cron-1"})
	if err != nil || get.GetFound() {
		t.Fatalf("deleted get=%#v err=%v", get, err)
	}
}
