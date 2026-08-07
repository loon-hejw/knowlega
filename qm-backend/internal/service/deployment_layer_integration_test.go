package service

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"

	deploymentv1 "github.com/hejw/qm-backend/api/qm/deployment/v1"
	"github.com/hejw/qm-backend/internal/data"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestDeploymentLayerServicePersistsDurableMapOverGRPC(t *testing.T) {
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
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE deployment_layer"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "DELETE FROM durable_map_versions WHERE tbl='deployment_layer'"); err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	deploymentv1.RegisterDeploymentLayerServiceServer(grpcServer, NewDeploymentLayerService(data.NewDeploymentLayerRepository(pg)))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := deploymentv1.NewDeploymentLayerServiceClient(connection)

	initial, err := client.Get(ctx, &deploymentv1.GetRequest{})
	if err != nil || initial.GetFound() || len(initial.GetPayload()) != 0 {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	first := []byte(`{"contentHash":"a","version":1,"bundle":{"contract":1,"tools":[],"skills":[]}}`)
	inserted, err := client.PutIfAbsent(ctx, &deploymentv1.PutIfAbsentRequest{Payload: first})
	if err != nil || string(inserted.GetPayload()) == "" {
		t.Fatalf("insert=%#v err=%v", inserted, err)
	}
	second := []byte(`{"contentHash":"other","version":1,"bundle":{"contract":1,"tools":[],"skills":[]}}`)
	already, err := client.PutIfAbsent(ctx, &deploymentv1.PutIfAbsentRequest{Payload: second})
	if err != nil || !sameJSON(already.GetPayload(), inserted.GetPayload()) {
		t.Fatalf("put-if-absent=%#v err=%v", already, err)
	}
	conflict, err := client.CompareAndSet(ctx, &deploymentv1.CompareAndSetRequest{ExpectedPayload: second, Payload: []byte(`{"contentHash":"b","version":2}`)})
	if err != nil || !conflict.GetFound() || conflict.GetUpdated() || !sameJSON(conflict.GetPayload(), inserted.GetPayload()) {
		t.Fatalf("conflict=%#v err=%v", conflict, err)
	}
	// JSONB equality accepts a reordered representation of the stored document.
	reordered := []byte(`{"bundle":{"skills":[],"tools":[],"contract":1},"version":1,"contentHash":"a"}`)
	next := []byte(`{"contentHash":"b","version":2,"bundle":{"contract":1,"tools":[],"skills":[]}}`)
	updated, err := client.CompareAndSet(ctx, &deploymentv1.CompareAndSetRequest{ExpectedPayload: reordered, Payload: next})
	if err != nil || !updated.GetFound() || !updated.GetUpdated() || !sameJSON(updated.GetPayload(), next) {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	read, err := client.Get(ctx, &deploymentv1.GetRequest{})
	if err != nil || !read.GetFound() || !sameJSON(read.GetPayload(), next) {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	if _, err := client.CompareAndSet(ctx, &deploymentv1.CompareAndSetRequest{ExpectedPayload: []byte(`not-json`), Payload: next}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid payload error=%v", err)
	}
	var version int64
	if err := pg.Pool.QueryRow(ctx, "SELECT v FROM durable_map_versions WHERE tbl='deployment_layer'").Scan(&version); err != nil || version != 4 {
		t.Fatalf("durable map version=%d err=%v", version, err)
	}
}

func sameJSON(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}
