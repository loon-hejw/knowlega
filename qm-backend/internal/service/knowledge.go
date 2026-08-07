package service

import (
	"context"
	"strings"

	knowledgev1 "github.com/hejw/qm-backend/api/knowledge/v1"
	"github.com/hejw/qm-backend/internal/knowledge"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type KnowledgeService struct {
	knowledgev1.UnimplementedKnowledgeCoreServer
	engine        *knowledge.Engine
	queryAgent    knowledge.QueryAgent
	compilerAgent knowledge.CompilerAgent
	defaultOrgID  string
}

func NewKnowledgeService(engine *knowledge.Engine, defaultOrgID string, queryAgent knowledge.QueryAgent, compilerAgent knowledge.CompilerAgent) *KnowledgeService {
	return &KnowledgeService{engine: engine, defaultOrgID: defaultOrgID, queryAgent: queryAgent, compilerAgent: compilerAgent}
}

func (s *KnowledgeService) EnsureScope(ctx context.Context, request *knowledgev1.EnsureScopeRequest) (*knowledgev1.ScopeStatus, error) {
	ref, err := s.scopeFor(request.GetScope(), request.GetOrgId(), nil)
	if err != nil {
		return nil, err
	}
	result, err := s.engine.EnsureScope(ctx, knowledge.ScopeRef{OrgID: ref.OrgID, ExternalScopeID: ref.ExternalScopeID, Kind: ref.Kind, Name: request.GetName()})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return toScopeStatus(result), nil
}

func (s *KnowledgeService) GetStatus(ctx context.Context, request *knowledgev1.GetStatusRequest) (*knowledgev1.ScopeStatus, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	result, err := s.engine.GetStatus(ctx, ref)
	if err != nil {
		return nil, knowledgeError(err)
	}
	return toScopeStatus(result), nil
}

func (s *KnowledgeService) Search(ctx context.Context, request *knowledgev1.SearchRequest) (*knowledgev1.SearchResponse, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	results, err := s.engine.Search(ctx, ref, request.GetQuery(), int(request.GetLimit()))
	if err != nil {
		return nil, knowledgeError(err)
	}
	reply := &knowledgev1.SearchResponse{Results: make([]*knowledgev1.QueryResult, 0, len(results))}
	for _, result := range results {
		reply.Results = append(reply.Results, &knowledgev1.QueryResult{Path: result.Path, Title: result.Title, Snippet: result.Snippet, Score: int32(result.Score), Kind: result.Kind})
	}
	return reply, nil
}

func (s *KnowledgeService) ReadDocument(ctx context.Context, request *knowledgev1.ReadDocumentRequest) (*knowledgev1.Document, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	document, err := s.engine.ReadDocument(ctx, ref, request.GetPath())
	if err != nil {
		return nil, knowledgeError(err)
	}
	return &knowledgev1.Document{Path: document.Path, Title: document.Title, Kind: document.Kind, Content: document.Content}, nil
}

func (s *KnowledgeService) Query(ctx context.Context, request *knowledgev1.QueryRequest) (*knowledgev1.QueryResponse, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	result, err := s.engine.Query(ctx, ref, request.GetQuestion(), request.GetConversationContext(), int(request.GetLimit()), s.queryAgent)
	if err != nil {
		if strings.Contains(err.Error(), "requires an LLM") {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, knowledgeError(err)
	}
	reply := &knowledgev1.QueryResponse{Question: result.Question, Answer: result.Answer, Status: "ok", SuggestedWritebackTitle: result.SuggestedWritebackTitle, IncompleteReason: result.IncompleteReason, QueryId: result.ID, QueryPlan: result.QueryPlan, CanWriteBack: result.CanWriteBack}
	for _, citation := range result.Citations {
		reply.Citations = append(reply.Citations, &knowledgev1.QueryCitation{Path: citation.Path, Title: citation.Title, Kind: citation.Kind})
	}
	for _, searchResult := range result.SearchResults {
		reply.Results = append(reply.Results, &knowledgev1.QueryResult{Path: searchResult.Path, Title: searchResult.Title, Snippet: searchResult.Snippet, Score: int32(searchResult.Score), Kind: searchResult.Kind})
	}
	return reply, nil
}

func (s *KnowledgeService) SaveQueryAnswer(ctx context.Context, request *knowledgev1.SaveQueryAnswerRequest) (*knowledgev1.SaveQueryAnswerReply, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	path, err := s.engine.SaveQueryAnswer(ctx, ref, request.GetQueryId(), request.GetTitle())
	if err != nil {
		if strings.Contains(err.Error(), "cannot be written back") {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, knowledgeError(err)
	}
	return &knowledgev1.SaveQueryAnswerReply{Path: path}, nil
}

func (s *KnowledgeService) IngestSource(ctx context.Context, request *knowledgev1.IngestSourceRequest) (*knowledgev1.IngestSourceReply, error) {
	ref, err := s.scopeFor(request.GetScope(), "", request.GetCaller())
	if err != nil {
		return nil, err
	}
	result, err := s.engine.Ingest(ctx, ref, request.GetSourceName(), request.GetContent(), s.compilerAgent)
	if err != nil {
		if strings.Contains(err.Error(), "requires an LLM") {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, knowledgeError(err)
	}
	return &knowledgev1.IngestSourceReply{RawPath: result.RawPath, SkippedUnchanged: result.SkippedUnchanged, GeneratedPaths: result.GeneratedPaths, Reviews: result.Reviews}, nil
}

func (s *KnowledgeService) scopeFor(scope *knowledgev1.ScopeRef, requestedOrg string, caller *knowledgev1.CallerContext) (knowledge.ScopeRef, error) {
	if scope == nil || strings.TrimSpace(scope.GetExternalScopeId()) == "" || strings.TrimSpace(scope.GetKind()) == "" {
		return knowledge.ScopeRef{}, status.Error(codes.InvalidArgument, "scope is required")
	}
	if caller != nil && strings.TrimSpace(caller.GetScopeId()) != "" && strings.TrimSpace(caller.GetScopeId()) != strings.TrimSpace(scope.GetExternalScopeId()) {
		return knowledge.ScopeRef{}, status.Error(codes.PermissionDenied, "caller scope does not match requested scope")
	}
	orgID := strings.TrimSpace(requestedOrg)
	if orgID == "" && caller != nil {
		orgID = strings.TrimSpace(caller.GetOrgId())
	}
	if orgID == "" {
		orgID = s.defaultOrgID
	}
	return knowledge.ScopeRef{OrgID: orgID, ExternalScopeID: scope.GetExternalScopeId(), Kind: scope.GetKind()}, nil
}

func toScopeStatus(result knowledge.ScopeStatus) *knowledgev1.ScopeStatus {
	return &knowledgev1.ScopeStatus{Scope: &knowledgev1.ScopeRef{ExternalScopeId: result.Scope.ExternalScopeID, Kind: result.Scope.Kind}, ProjectId: result.Scope.ProjectID, ProjectName: result.Scope.ProjectName, Status: result.Scope.Status, WikiPageCount: result.WikiPageCount, SourceCount: result.SourceCount, Ready: result.Ready}
}

func knowledgeError(err error) error {
	if knowledge.IsMissing(err) {
		return status.Error(codes.NotFound, "knowledge scope was not found")
	}
	if strings.Contains(err.Error(), "path must") || strings.Contains(err.Error(), "escapes workspace") || strings.Contains(err.Error(), "required") {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
