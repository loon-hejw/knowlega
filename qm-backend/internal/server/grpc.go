package server

import (
	"context"
	"crypto/subtle"
	"time"

	kratosgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	knowledgev1 "github.com/hejw/qm-backend/api/knowledge/v1"
	controlv1 "github.com/hejw/qm-backend/api/qm/control/v1"
	cronv1 "github.com/hejw/qm-backend/api/qm/cron/v1"
	deploymentv1 "github.com/hejw/qm-backend/api/qm/deployment/v1"
	runnerv1 "github.com/hejw/qm-backend/api/qm/runner/v1"
	"github.com/hejw/qm-backend/internal/biz"
	"github.com/hejw/qm-backend/internal/config"
	"github.com/hejw/qm-backend/internal/data"
	"github.com/hejw/qm-backend/internal/knowledge"
	"github.com/hejw/qm-backend/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func NewGRPCServer(cfg config.Config, projects *biz.ProjectUsecase, runs *data.RunRepository, crons *data.CronRepository, layers *data.DeploymentLayerRepository, engine *knowledge.Engine, queryAgent knowledge.QueryAgent, compilerAgent knowledge.CompilerAgent) *kratosgrpc.Server {
	options := []kratosgrpc.ServerOption{kratosgrpc.Address(cfg.Server.GRPCAddr)}
	if cfg.Auth.GRPCInternalToken != "" {
		options = append(options, kratosgrpc.UnaryInterceptor(internalTokenInterceptor(cfg.Auth.GRPCInternalToken)))
	}
	server := kratosgrpc.NewServer(options...)
	controlv1.RegisterControlServiceServer(server, service.NewControlService(projects))
	runnerv1.RegisterRunnerServiceServer(server, service.NewRunnerService(runs, time.Duration(cfg.Runner.LeaseTTLSeconds)*time.Second, cfg.Runner.MaxClaims))
	knowledgev1.RegisterKnowledgeCoreServer(server, service.NewKnowledgeService(engine, cfg.QM.OrgID, queryAgent, compilerAgent))
	cronv1.RegisterCronServiceServer(server, service.NewCronService(crons))
	deploymentv1.RegisterDeploymentLayerServiceServer(server, service.NewDeploymentLayerService(layers))
	return server
}

func internalTokenInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == controlv1.ControlService_Health_FullMethodName {
			return handler(ctx, request)
		}
		metadata, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "internal token required")
		}
		values := append(metadata.Get("x-qm-backend-internal-token"), metadata.Get("authorization")...)
		valid := false
		for _, value := range values {
			if subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1 || subtle.ConstantTimeCompare([]byte(value), []byte("Bearer "+token)) == 1 {
				valid = true
				break
			}
		}
		if !valid {
			return nil, status.Error(codes.Unauthenticated, "internal token required")
		}
		return handler(ctx, request)
	}
}
