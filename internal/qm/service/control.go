package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	controlv1 "github.com/loon-hejw/knowlega/api/qm/control/v1"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ControlService struct {
	controlv1.UnimplementedControlServiceServer
	projects *biz.ProjectUsecase
	config   config.Config
}

func NewControlService(projects *biz.ProjectUsecase, cfg ...config.Config) *ControlService {
	service := &ControlService{projects: projects}
	if len(cfg) > 0 {
		service.config = cfg[0]
	}
	return service
}

func (s *ControlService) GetRuntimeConfiguration(context.Context, *controlv1.RuntimeConfigurationRequest) (*controlv1.RuntimeConfigurationReply, error) {
	models := s.config.QM.Models
	reply := &controlv1.RuntimeConfigurationReply{
		DefaultHarness: models.DefaultHarness,
		Request: &controlv1.ModelRequestConfig{
			TimeoutSeconds: int32(models.Request.TimeoutSeconds), Retries: int32(models.Request.Retries),
			MaxInputChars: int32(models.Request.MaxInputChars), MaxOutputTokens: int32(models.Request.MaxOutputTokens),
			DisableThinking: models.Request.DisableThinking,
		},
		Slack: &controlv1.SlackRuntimeConfig{
			BotToken: s.config.QM.Slack.BotToken, AppToken: s.config.QM.Slack.AppToken,
			TeamId: s.config.QM.Slack.TeamID, TeamName: s.config.QM.Slack.TeamName,
		},
	}
	for _, harness := range models.Harnesses {
		reply.Harnesses = append(reply.Harnesses, &controlv1.RuntimeHarness{
			Id: harness.ID, Provider: harness.Provider, ModelIds: harness.ModelIDs, DefaultModel: harness.DefaultModel,
		})
	}
	for _, provider := range models.Providers {
		item := &controlv1.ModelProvider{
			Id: provider.ID, Protocol: provider.Protocol, BaseUrl: provider.BaseURL, ApiKey: provider.APIKey,
			UserAgent: provider.UserAgent, AnthropicVersion: provider.AnthropicVersion,
		}
		for _, model := range provider.Models {
			item.Models = append(item.Models, &controlv1.RuntimeModel{
				Id: model.ID, Name: model.Name, ContextWindow: int32(model.ContextWindow), MaxTokens: int32(model.MaxTokens),
			})
		}
		reply.Providers = append(reply.Providers, item)
	}
	for _, client := range s.config.QM.OAuth.Clients {
		reply.OauthClients = append(reply.OauthClients, &controlv1.OAuthClientConfig{
			Provider: client.Provider, ClientId: client.ClientID, ClientSecret: client.ClientSecret,
			Scopes: client.Scopes, RedirectAllowlist: client.RedirectAllowlist,
			ConsentMode: client.ConsentMode, HostedDomain: client.HostedDomain,
		})
	}
	// Hash the runtime payload itself so secret-only YAML changes also trigger
	// Node adapter reconciliation. Public config types intentionally omit secrets
	// from JSON, so hashing those structs would miss credential rotation.
	revisionJSON, _ := json.Marshal(reply)
	revisionHash := sha256.Sum256(revisionJSON)
	reply.Revision = hex.EncodeToString(revisionHash[:])
	return reply, nil
}

func (s *ControlService) Health(context.Context, *controlv1.HealthRequest) (*controlv1.HealthReply, error) {
	return &controlv1.HealthReply{Ok: true}, nil
}

func (s *ControlService) ListProjects(ctx context.Context, request *controlv1.ListProjectsRequest) (*controlv1.ListProjectsReply, error) {
	caller, err := requireCaller(request.GetCaller())
	if err != nil {
		return nil, err
	}
	projects, err := s.projects.List(ctx, caller.ActorId)
	if err != nil {
		return nil, invalidArgument(err)
	}
	result := make([]*controlv1.Project, 0, len(projects))
	for _, project := range projects {
		result = append(result, toProtoProject(project))
	}
	return &controlv1.ListProjectsReply{Projects: result}, nil
}

func (s *ControlService) CreateProject(ctx context.Context, request *controlv1.CreateProjectRequest) (*controlv1.ProjectReply, error) {
	caller, err := requireCaller(request.GetCaller())
	if err != nil {
		return nil, err
	}
	project, err := s.projects.Create(ctx, caller.ActorId, request.GetName())
	if err != nil {
		return nil, invalidArgument(err)
	}
	if project == nil {
		return nil, status.Error(codes.PermissionDenied, "principal is no longer active")
	}
	return &controlv1.ProjectReply{Project: toProtoProject(*project)}, nil
}

func (s *ControlService) AddProjectMember(ctx context.Context, request *controlv1.ProjectMemberRequest) (*controlv1.ProjectReply, error) {
	caller, err := requireCaller(request.GetCaller())
	if err != nil {
		return nil, err
	}
	result, err := s.projects.AddMember(ctx, request.GetProjectId(), caller.ActorId, request.GetMemberId())
	if err != nil {
		return nil, err
	}
	return projectMutation(result)
}

func (s *ControlService) RemoveProjectMember(ctx context.Context, request *controlv1.ProjectMemberRequest) (*controlv1.ProjectReply, error) {
	caller, err := requireCaller(request.GetCaller())
	if err != nil {
		return nil, err
	}
	result, err := s.projects.RemoveMember(ctx, request.GetProjectId(), caller.ActorId, request.GetMemberId())
	if err != nil {
		return nil, err
	}
	return projectMutation(result)
}

func requireCaller(caller *controlv1.CallerContext) (*controlv1.CallerContext, error) {
	if caller == nil || caller.GetActorId() == "" || caller.GetScopeId() == "" {
		return nil, status.Error(codes.Unauthenticated, "caller actor_id and scope_id are required")
	}
	return caller, nil
}

func projectMutation(result biz.ProjectMutation) (*controlv1.ProjectReply, error) {
	switch result.Status {
	case "ok":
		return &controlv1.ProjectReply{Project: toProtoProject(*result.Project)}, nil
	case "not_found":
		return nil, status.Error(codes.NotFound, "project not found")
	case "forbidden":
		return nil, status.Error(codes.PermissionDenied, "project membership mutation forbidden")
	case "invalid_member":
		return nil, status.Error(codes.InvalidArgument, "invalid project member")
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid project mutation")
	}
}

func invalidArgument(err error) error {
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func toProtoProject(project biz.Project) *controlv1.Project {
	return &controlv1.Project{Id: project.ID, OrgId: project.OrgID, Name: project.Name, OwnerId: project.OwnerID, MemberIds: project.MemberIDs, CreatedAt: project.CreatedAt, UpdatedAt: project.UpdatedAt}
}
