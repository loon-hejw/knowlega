package service

import (
	"context"
	"errors"

	controlv1 "github.com/loon-hejw/knowlega/api/qm/control/v1"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ControlService struct {
	controlv1.UnimplementedControlServiceServer
	projects *biz.ProjectUsecase
}

func NewControlService(projects *biz.ProjectUsecase) *ControlService {
	return &ControlService{projects: projects}
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
