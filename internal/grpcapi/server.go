package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
	v1 "github.com/hejw/knowledge-core/internal/grpcapi/knowledge/v1"
	"github.com/hejw/knowledge-core/internal/scope"
	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type ProjectDocumentCounter interface {
	CountProjectDocuments(context.Context, string) (pages, sources int64, err error)
}

type ServerOptions struct {
	Resolver          *scope.Resolver
	SearchStore       service.SearchEvidenceStore
	GraphStore        service.GraphEvidenceStore
	QueryLogStore     service.QueryLogStore
	QueryAgent        service.QueryAgent
	EmbeddingProvider service.EmbeddingProvider
	Runtime           service.QueryRuntimeOptions
	Bootstrap         *service.BootstrapTracker
	DocumentCounter   ProjectDocumentCounter
	AuthToken         string
	RequireAuth       bool
	TransportCreds    credentials.TransportCredentials
}

type Server struct {
	v1.UnimplementedKnowledgeCoreServer
	resolver          *scope.Resolver
	searchStore       service.SearchEvidenceStore
	graphStore        service.GraphEvidenceStore
	queryLogStore     service.QueryLogStore
	queryAgent        service.QueryAgent
	embeddingProvider service.EmbeddingProvider
	runtime           service.QueryRuntimeOptions
	bootstrap         *service.BootstrapTracker
	documentCounter   ProjectDocumentCounter
	authToken         string
	requireAuth       bool
	transportCreds    credentials.TransportCredentials
}

func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Resolver == nil {
		return nil, fmt.Errorf("grpc server requires a scope resolver")
	}
	return &Server{
		resolver:          opts.Resolver,
		searchStore:       opts.SearchStore,
		graphStore:        opts.GraphStore,
		queryLogStore:     opts.QueryLogStore,
		queryAgent:        opts.QueryAgent,
		embeddingProvider: opts.EmbeddingProvider,
		runtime:           opts.Runtime,
		bootstrap:         opts.Bootstrap,
		documentCounter:   opts.DocumentCounter,
		authToken:         strings.TrimSpace(opts.AuthToken),
		requireAuth:       opts.RequireAuth,
		transportCreds:    opts.TransportCreds,
	}, nil
}

func (s *Server) Register(server grpc.ServiceRegistrar) {
	v1.RegisterKnowledgeCoreServer(server, s)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	serverOptions := []grpc.ServerOption{grpc.UnaryInterceptor(s.unaryAuthInterceptor)}
	if s.transportCreds != nil {
		serverOptions = append(serverOptions, grpc.Creds(s.transportCreds))
	}
	grpcServer := grpc.NewServer(serverOptions...)
	s.Register(grpcServer)
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	return grpcServer.Serve(listener)
}

func (s *Server) unaryAuthInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.authorizeMetadata(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) authorizeMetadata(ctx context.Context) error {
	if s.authToken == "" && !s.requireAuth {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "knowledge core authentication metadata is required")
	}
	for _, value := range append(md.Get("authorization"), md.Get("x-kbcore-token")...) {
		value = strings.TrimSpace(value)
		if value == s.authToken || value == "Bearer "+s.authToken {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid knowledge core service token")
}

func (s *Server) EnsureScope(ctx context.Context, req *v1.EnsureScopeRequest) (*v1.ScopeStatus, error) {
	if req == nil || req.GetScope() == nil {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}
	if err := s.checkCallerScope(req.GetScope(), nil); err != nil {
		return nil, err
	}
	binding, err := s.resolver.Ensure(ctx, req.GetScope().GetExternalScopeId(), req.GetScope().GetKind(), req.GetOrgId(), req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return s.statusFor(ctx, binding), nil
}

func (s *Server) GetStatus(ctx context.Context, req *v1.GetStatusRequest) (*v1.ScopeStatus, error) {
	if req == nil || req.GetScope() == nil {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}
	if err := s.checkCallerScope(req.GetScope(), req.GetCaller()); err != nil {
		return nil, err
	}
	binding, err := s.resolver.Resolve(ctx, req.GetScope().GetExternalScopeId(), req.GetScope().GetKind())
	if err != nil {
		return nil, grpcError(err)
	}
	return s.statusFor(ctx, binding), nil
}

func (s *Server) Query(ctx context.Context, req *v1.QueryRequest) (*v1.QueryResponse, error) {
	if req == nil || req.GetScope() == nil {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}
	if strings.TrimSpace(req.GetQuestion()) == "" {
		return nil, status.Error(codes.InvalidArgument, "question is required")
	}
	if err := s.checkCallerScope(req.GetScope(), req.GetCaller()); err != nil {
		return nil, err
	}
	binding, err := s.resolver.Resolve(ctx, req.GetScope().GetExternalScopeId(), req.GetScope().GetKind())
	if err != nil {
		return nil, grpcError(err)
	}
	answer, err := service.QueryLLMWikiWithOptions(service.QueryOptions{
		ProjectPath:         binding.RootPath,
		ProjectID:           binding.ProjectID,
		Question:            strings.TrimSpace(req.GetQuestion()),
		ConversationContext: req.GetConversationContext(),
		Limit:               int(req.GetLimit()),
		Agent:               s.queryAgent,
		SearchStore:         s.searchStore,
		GraphStore:          s.graphStore,
		QueryLogStore:       s.queryLogStore,
		EmbeddingProvider:   s.embeddingProvider,
		Context:             ctx,
		Runtime:             s.runtime,
	})
	if err != nil {
		return nil, grpcError(err)
	}
	response := &v1.QueryResponse{
		Question:                answer.Question,
		Answer:                  answer.Answer,
		Status:                  answer.Status,
		SuggestedWritebackTitle: answer.SuggestedWritebackTitle,
		IncompleteReason:        answer.IncompleteReason,
	}
	for _, citation := range answer.Citations {
		response.Citations = append(response.Citations, &v1.QueryCitation{Path: citation.Path, Title: citation.Title, Kind: citation.Kind})
	}
	for _, result := range answer.Results {
		response.Results = append(response.Results, &v1.QueryResult{Path: result.Path, Title: result.Title, Snippet: result.Snippet, Score: int32(result.Score), Kind: result.Kind})
	}
	return response, nil
}

func (s *Server) Search(ctx context.Context, req *v1.SearchRequest) (*v1.SearchResponse, error) {
	if req == nil || req.GetScope() == nil {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}
	if strings.TrimSpace(req.GetQuery()) == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	if err := s.checkCallerScope(req.GetScope(), req.GetCaller()); err != nil {
		return nil, err
	}
	binding, err := s.resolver.Resolve(ctx, req.GetScope().GetExternalScopeId(), req.GetScope().GetKind())
	if err != nil {
		return nil, grpcError(err)
	}
	results, err := service.SearchProjectDocuments(ctx, binding.RootPath, binding.ProjectID, req.GetQuery(), int(req.GetLimit()), s.searchStore, s.embeddingProvider)
	if err != nil {
		return nil, grpcError(err)
	}
	response := &v1.SearchResponse{}
	for _, result := range results {
		response.Results = append(response.Results, &v1.QueryResult{Path: result.Path, Title: result.Title, Snippet: result.Snippet, Score: int32(result.Score), Kind: result.Kind})
	}
	return response, nil
}

func (s *Server) ReadDocument(ctx context.Context, req *v1.ReadDocumentRequest) (*v1.Document, error) {
	if req == nil || req.GetScope() == nil {
		return nil, status.Error(codes.InvalidArgument, "scope is required")
	}
	if strings.TrimSpace(req.GetPath()) == "" {
		return nil, status.Error(codes.InvalidArgument, "path is required")
	}
	requestedPath := strings.ReplaceAll(strings.TrimSpace(req.GetPath()), "\\", "/")
	if !strings.HasPrefix(requestedPath, "wiki/") && !strings.HasPrefix(requestedPath, "raw/sources/") {
		return nil, status.Error(codes.InvalidArgument, "path must be under wiki/ or raw/sources/")
	}
	if err := s.checkCallerScope(req.GetScope(), req.GetCaller()); err != nil {
		return nil, err
	}
	binding, err := s.resolver.Resolve(ctx, req.GetScope().GetExternalScopeId(), req.GetScope().GetKind())
	if err != nil {
		return nil, grpcError(err)
	}
	doc, err := service.ReadProjectDocument(binding.RootPath, req.GetPath())
	if err != nil {
		return nil, grpcError(err)
	}
	return &v1.Document{Path: doc.Path, Title: doc.Title, Kind: doc.Kind, Content: doc.Content}, nil
}

func (s *Server) checkCallerScope(ref *v1.ScopeRef, caller *v1.CallerContext) error {
	if caller == nil || strings.TrimSpace(caller.GetScopeId()) == "" || ref == nil {
		return nil
	}
	if strings.TrimSpace(caller.GetScopeId()) != strings.TrimSpace(ref.GetExternalScopeId()) {
		return status.Error(codes.PermissionDenied, "caller scope does not match requested scope")
	}
	return nil
}

func (s *Server) statusFor(ctx context.Context, binding core.ScopeBinding) *v1.ScopeStatus {
	ready := binding.Status == "active"
	if _, err := os.Stat(binding.RootPath); err != nil {
		ready = false
	}
	pages, sources := int64(0), int64(0)
	if s.documentCounter != nil {
		pages, sources, _ = s.documentCounter.CountProjectDocuments(ctx, binding.ProjectID)
	} else {
		if scanned, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: binding.RootPath, ProjectID: binding.ProjectID}); err == nil {
			pages = int64(len(scanned))
		}
		_ = filepath.WalkDir(filepath.Join(binding.RootPath, "raw", "sources"), func(_ string, entry os.DirEntry, err error) error {
			if err == nil && entry != nil && !entry.IsDir() {
				sources++
			}
			return nil
		})
	}
	if s.bootstrap != nil {
		ready = ready && s.bootstrap.Ready()
	}
	return &v1.ScopeStatus{
		Scope:         &v1.ScopeRef{ExternalScopeId: binding.ExternalScopeID, Kind: binding.Kind},
		ProjectId:     binding.ProjectID,
		ProjectName:   binding.ProjectName,
		Status:        binding.Status,
		WikiPageCount: pages,
		SourceCount:   sources,
		Ready:         ready,
	}
}

func grpcError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	if errors.Is(err, scope.ErrNotFound) || strings.Contains(err.Error(), scope.ErrNotFound.Error()) {
		return status.Error(codes.NotFound, err.Error())
	}
	lowerMessage := strings.ToLower(err.Error())
	if strings.Contains(lowerMessage, "not found") || strings.Contains(lowerMessage, "found no content") {
		return status.Error(codes.NotFound, err.Error())
	}
	if errors.Is(err, scope.ErrInvalid) || errors.Is(err, scope.ErrNotConfigured) || strings.Contains(err.Error(), "must be under wiki/") || strings.Contains(err.Error(), "must be under raw/") {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
