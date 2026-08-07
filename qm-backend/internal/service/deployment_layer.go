package service

import (
	"context"
	"encoding/json"

	deploymentv1 "github.com/hejw/qm-backend/api/qm/deployment/v1"
	"github.com/hejw/qm-backend/internal/data"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type DeploymentLayerService struct {
	deploymentv1.UnimplementedDeploymentLayerServiceServer
	layers *data.DeploymentLayerRepository
}

func NewDeploymentLayerService(layers *data.DeploymentLayerRepository) *DeploymentLayerService {
	return &DeploymentLayerService{layers: layers}
}

func (s *DeploymentLayerService) Get(ctx context.Context, _ *deploymentv1.GetRequest) (*deploymentv1.GetReply, error) {
	payload, found, err := s.layers.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &deploymentv1.GetReply{Found: found, Payload: payload}, nil
}

func (s *DeploymentLayerService) PutIfAbsent(ctx context.Context, request *deploymentv1.PutIfAbsentRequest) (*deploymentv1.PutIfAbsentReply, error) {
	payload, err := s.layers.PutIfAbsent(ctx, json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &deploymentv1.PutIfAbsentReply{Payload: payload}, nil
}

func (s *DeploymentLayerService) CompareAndSet(ctx context.Context, request *deploymentv1.CompareAndSetRequest) (*deploymentv1.CompareAndSetReply, error) {
	payload, found, updated, err := s.layers.CompareAndSet(ctx, json.RawMessage(request.GetExpectedPayload()), json.RawMessage(request.GetPayload()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &deploymentv1.CompareAndSetReply{Found: found, Updated: updated, Payload: payload}, nil
}
