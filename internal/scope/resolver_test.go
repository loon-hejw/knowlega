package scope

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/core"
)

type memoryStore struct {
	projects map[string]core.Project
	bindings map[string]core.ScopeBinding
}

func newMemoryStore() *memoryStore {
	return &memoryStore{projects: map[string]core.Project{}, bindings: map[string]core.ScopeBinding{}}
}

func (s *memoryStore) UpsertProject(_ context.Context, project core.Project) error {
	s.projects[project.ID] = project
	return nil
}

func (s *memoryStore) UpsertScopeBinding(_ context.Context, binding core.ScopeBinding) error {
	s.bindings[binding.Provider+"\x00"+binding.ExternalScopeID] = binding
	return nil
}

func (s *memoryStore) GetScopeBinding(_ context.Context, provider, externalID string) (core.ScopeBinding, error) {
	binding, ok := s.bindings[provider+"\x00"+externalID]
	if !ok {
		return core.ScopeBinding{}, errors.New("sql: no rows in result set")
	}
	return binding, nil
}

func TestResolverEnsureDerivesServerOwnedProjectRoot(t *testing.T) {
	root := t.TempDir()
	store := newMemoryStore()
	resolver, err := NewResolver(Options{Provider: "qm", ScopeRoot: root, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolver.Ensure(context.Background(), "group:project:alpha", "project", "org-1", "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	if binding.ProjectID == "" || binding.RootPath == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	if filepath.Dir(binding.RootPath) != root {
		t.Fatalf("root escaped scope root: %s", binding.RootPath)
	}
	if _, err := resolver.Resolve(context.Background(), "group:project:alpha", "project"); err != nil {
		t.Fatalf("resolve persisted binding: %v", err)
	}
	if _, err := resolver.Ensure(context.Background(), "../escape", "project", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid external scope id, got %v", err)
	}
}

func TestResolverDoesNotCrossScopeBindings(t *testing.T) {
	resolver, err := NewResolver(Options{Provider: "qm", ScopeRoot: t.TempDir(), Store: newMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolver.Ensure(context.Background(), "group:project:first", "project", "", "First")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Ensure(context.Background(), "group:project:second", "project", "", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectID == second.ProjectID || first.RootPath == second.RootPath {
		t.Fatalf("scope bindings collided: first=%+v second=%+v", first, second)
	}
	if _, err := resolver.Resolve(context.Background(), "group:project:missing", "project"); !errors.Is(err, ErrNotFound) && !containsError(err, ErrNotFound) {
		t.Fatalf("expected missing scope, got %v", err)
	}
}

func containsError(err, target error) bool {
	return err != nil && target != nil && (errors.Is(err, target) || strings.Contains(err.Error(), target.Error()))
}
