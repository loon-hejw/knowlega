package grpcapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	v1 "github.com/hejw/knowledge-core/internal/grpcapi/knowledge/v1"
	"github.com/hejw/knowledge-core/internal/scope"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestReadDocumentUsesResolvedScopeAndRejectsCrossScopeCaller(t *testing.T) {
	root := t.TempDir()
	pagePath := filepath.Join(root, "wiki", "concepts", "scope.md")
	if err := os.MkdirAll(filepath.Dir(pagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pagePath, []byte("---\ntitle: Scope\n---\nOnly project evidence."), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := scope.NewResolver(scope.Options{Provider: "qm", ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Register(context.Background(), core.ScopeBinding{
		Provider: "qm", ExternalScopeID: "group:project:one", Kind: "project",
		ProjectID: "one", ProjectName: "One", RootPath: root, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerOptions{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ReadDocument(context.Background(), &v1.ReadDocumentRequest{
		Scope: &v1.ScopeRef{ExternalScopeId: "group:project:one", Kind: "project"},
		Path:  "wiki/concepts/scope.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetPath() != "wiki/concepts/scope.md" || response.GetContent() == "" {
		t.Fatalf("unexpected document response: %+v", response)
	}
	_, err = server.ReadDocument(context.Background(), &v1.ReadDocumentRequest{
		Scope:  &v1.ScopeRef{ExternalScopeId: "group:project:one", Kind: "project"},
		Caller: &v1.CallerContext{ScopeId: "group:project:two"},
		Path:   "wiki/concepts/scope.md",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
	_, err = server.ReadDocument(context.Background(), &v1.ReadDocumentRequest{
		Scope: &v1.ScopeRef{ExternalScopeId: "group:project:one", Kind: "project"},
		Path:  "../outside.md",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument for path traversal, got %v", err)
	}
	_, err = server.ReadDocument(context.Background(), &v1.ReadDocumentRequest{
		Scope: &v1.ScopeRef{ExternalScopeId: "group:project:one", Kind: "project"},
		Path:  "wiki/concepts/missing.md",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected not found for missing document, got %v", err)
	}
}

func TestGRPCReadDocumentRoundTrip(t *testing.T) {
	root := t.TempDir()
	pagePath := filepath.Join(root, "wiki", "roundtrip.md")
	if err := os.MkdirAll(filepath.Dir(pagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pagePath, []byte("---\ntitle: Roundtrip\n---\ncontent"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := scope.NewResolver(scope.Options{Provider: "qm", ScopeRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Register(context.Background(), core.ScopeBinding{
		Provider: "qm", ExternalScopeID: "group:project:wire", Kind: "project",
		ProjectID: "wire", ProjectName: "Wire", RootPath: root, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewServer(ServerOptions{Resolver: resolver, AuthToken: "secret", RequireAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- service.Serve(ctx, listener) }()
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := v1.NewKnowledgeCoreClient(conn)
	callCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer secret")
	response, err := client.ReadDocument(callCtx, &v1.ReadDocumentRequest{
		Scope: &v1.ScopeRef{ExternalScopeId: "group:project:wire", Kind: "project"},
		Path:  "wiki/roundtrip.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetContent() == "" {
		t.Fatalf("empty gRPC document: %+v", response)
	}
	_, err = client.ReadDocument(ctx, &v1.ReadDocumentRequest{
		Scope: &v1.ScopeRef{ExternalScopeId: "group:project:wire", Kind: "project"},
		Path:  "wiki/roundtrip.md",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated call, got %v", err)
	}
	cancel()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("gRPC server did not stop")
	}
}
