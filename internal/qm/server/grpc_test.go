package server

import (
	"context"
	"net"
	"testing"
	"time"

	controlv1 "github.com/loon-hejw/knowlega/api/qm/control/v1"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/service"
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
	cfg := config.Default()
	cfg.QM.Models.DefaultHarness = "pi"
	cfg.QM.Models.Providers = []config.ModelProviderConfig{{
		ID: "example", Protocol: "openai", BaseURL: "https://models.example/v1", APIKey: "provider-secret",
		Models: []config.ModelDefinition{{ID: "model-1", Name: "Model One"}},
	}}
	cfg.QM.Models.Harnesses = []config.ModelHarnessConfig{{ID: "pi", Provider: "example", ModelIDs: []string{"model-1"}, DefaultModel: "model-1"}}
	cfg.QM.Slack = config.SlackConfig{BotToken: "xoxb-secret", AppToken: "xapp-secret", TeamID: "T1"}
	cfg.QM.OAuth.Clients = []config.OAuthClientConfig{{Provider: "github", ClientID: "client-id", ClientSecret: "client-secret"}}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(internalTokenInterceptor("internal-secret")))
	controlv1.RegisterControlServiceServer(grpcServer, service.NewControlService(biz.NewProjectUsecase(testProjectRepo{}), cfg))
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
	_, err = client.GetRuntimeConfiguration(ctx, &controlv1.RuntimeConfigurationRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("runtime configuration must require the internal token, got %v", err)
	}
	runtimeConfig, err := client.GetRuntimeConfiguration(authenticated, &controlv1.RuntimeConfigurationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.GetDefaultHarness() != "pi" || runtimeConfig.GetHarnesses()[0].GetDefaultModel() != "model-1" || runtimeConfig.GetProviders()[0].GetApiKey() != "provider-secret" || runtimeConfig.GetSlack().GetBotToken() != "xoxb-secret" || runtimeConfig.GetOauthClients()[0].GetClientSecret() != "client-secret" || runtimeConfig.GetRevision() == "" {
		t.Fatalf("unexpected runtime configuration %#v", runtimeConfig)
	}
	rotated := cfg
	rotated.QM.Models.Providers = append([]config.ModelProviderConfig(nil), cfg.QM.Models.Providers...)
	rotated.QM.Models.Providers[0].APIKey = "rotated-provider-secret"
	rotatedConfig, err := service.NewControlService(biz.NewProjectUsecase(testProjectRepo{}), rotated).GetRuntimeConfiguration(ctx, &controlv1.RuntimeConfigurationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.GetRevision() == rotatedConfig.GetRevision() {
		t.Fatal("runtime configuration revision did not change after secret rotation")
	}
}
