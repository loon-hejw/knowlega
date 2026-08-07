package server

import (
	"context"
	"net"
	"testing"
	"time"

	controlv1 "github.com/hejw/qm-backend/api/qm/control/v1"
	"github.com/hejw/qm-backend/internal/biz"
	"github.com/hejw/qm-backend/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type testProjectRepo struct{}

func (testProjectRepo) Create(context.Context, string, string) (*biz.Project, error) {
	return &biz.Project{ID: "p1", OrgID: "acme", Name: "Roadmap", OwnerID: "alice", MemberIDs: []string{"alice"}, CreatedAt: 1, UpdatedAt: 1}, nil
}
func (testProjectRepo) ListForMember(context.Context, string) ([]biz.Project, error) { return nil, nil }
func (testProjectRepo) AddMember(context.Context, string, string, string) (biz.ProjectMutation, error) {
	return biz.ProjectMutation{}, nil
}
func (testProjectRepo) RemoveMember(context.Context, string, string, string) (biz.ProjectMutation, error) {
	return biz.ProjectMutation{}, nil
}
func (testProjectRepo) Rename(context.Context, string, string, string) (biz.ProjectMutation, error) {
	return biz.ProjectMutation{}, nil
}
func (testProjectRepo) HasScopeMembership(context.Context, string, string) (bool, error) {
	return false, nil
}

func TestControlGRPCRequiresInternalToken(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(internalTokenInterceptor("internal-secret")))
	controlv1.RegisterControlServiceServer(grpcServer, service.NewControlService(biz.NewProjectUsecase(testProjectRepo{})))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := controlv1.NewControlServiceClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.CreateProject(ctx, &controlv1.CreateProjectRequest{Caller: &controlv1.CallerContext{ActorId: "alice", ScopeId: "personal:alice"}, Name: "Roadmap"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
	authenticated := metadata.AppendToOutgoingContext(ctx, "x-qm-backend-internal-token", "internal-secret")
	result, err := client.CreateProject(authenticated, &controlv1.CreateProjectRequest{Caller: &controlv1.CallerContext{ActorId: "alice", ScopeId: "personal:alice"}, Name: "Roadmap"})
	if err != nil {
		t.Fatal(err)
	}
	if result.GetProject().GetId() != "p1" {
		t.Fatalf("unexpected project %#v", result.GetProject())
	}
}
