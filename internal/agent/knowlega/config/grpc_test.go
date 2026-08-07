package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGRPCDefaultsAreDisabled(t *testing.T) {
	cfg := Defaults()
	if cfg.Server.GRPC.Enabled {
		t.Fatal("gRPC integration must be opt-in")
	}
	if cfg.Server.GRPC.Addr != "127.0.0.1:19830" {
		t.Fatalf("unexpected gRPC default address: %q", cfg.Server.GRPC.Addr)
	}
}

func TestGRPCEnabledRequiresScopeRoot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.md")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	content := `project:
  name: demo
  path: project
  bootstrap:
    source: source.md
server:
  grpc:
    enabled: true
    addr: 127.0.0.1:19830
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "server.grpc.scope_root is required") {
		t.Fatalf("expected scope root validation error, got %v", err)
	}
}
