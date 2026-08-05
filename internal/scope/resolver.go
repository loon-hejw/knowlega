package scope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

var (
	ErrNotFound        = errors.New("scope binding not found")
	ErrInvalid         = errors.New("invalid scope")
	ErrNotConfigured   = errors.New("scope provisioning is not configured")
	ErrPathOutsideRoot = errors.New("scope path is outside configured root")
)

// BindingStore is deliberately smaller than the PostgreSQL store. This keeps
// the resolver usable in file-only tests and prevents the gRPC layer from
// depending on a concrete database implementation.
type BindingStore interface {
	UpsertProject(context.Context, core.Project) error
	UpsertScopeBinding(context.Context, core.ScopeBinding) error
	GetScopeBinding(context.Context, string, string) (core.ScopeBinding, error)
}

type Options struct {
	Provider       string
	ScopeRoot      string
	Store          BindingStore
	DefaultBinding *core.ScopeBinding
}

type Resolver struct {
	provider  string
	scopeRoot string
	store     BindingStore
	mu        sync.RWMutex
	bindings  map[string]core.ScopeBinding
}

func NewResolver(opts Options) (*Resolver, error) {
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" {
		provider = "qm"
	}
	if strings.ContainsAny(provider, "/\\\x00") {
		return nil, fmt.Errorf("%w: unsafe provider", ErrInvalid)
	}
	r := &Resolver{
		provider:  provider,
		scopeRoot: strings.TrimSpace(opts.ScopeRoot),
		store:     opts.Store,
		bindings:  make(map[string]core.ScopeBinding),
	}
	if r.scopeRoot != "" {
		abs, err := filepath.Abs(r.scopeRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve scope root: %w", err)
		}
		r.scopeRoot = filepath.Clean(abs)
	}
	if opts.DefaultBinding != nil {
		binding := normalizeBinding(*opts.DefaultBinding, r.provider)
		if err := validateBinding(binding); err != nil {
			return nil, err
		}
		r.bindings[binding.ExternalScopeID] = binding
	}
	return r, nil
}

func (r *Resolver) Provider() string { return r.provider }

func (r *Resolver) Resolve(ctx context.Context, externalScopeID, kind string) (core.ScopeBinding, error) {
	externalScopeID, kind, err := normalizeRef(externalScopeID, kind)
	if err != nil {
		return core.ScopeBinding{}, err
	}
	if r.store != nil {
		binding, storeErr := r.store.GetScopeBinding(ctx, r.provider, externalScopeID)
		if storeErr == nil {
			if binding.Kind != kind && kind != "" {
				return core.ScopeBinding{}, fmt.Errorf("%w: scope kind mismatch", ErrInvalid)
			}
			return validateResolved(binding)
		}
		if !errors.Is(storeErr, ErrNotFound) && !isNoRows(storeErr) {
			return core.ScopeBinding{}, storeErr
		}
	}
	r.mu.RLock()
	binding, ok := r.bindings[externalScopeID]
	r.mu.RUnlock()
	if !ok {
		return core.ScopeBinding{}, fmt.Errorf("%w: %s", ErrNotFound, externalScopeID)
	}
	return validateResolved(binding)
}

func (r *Resolver) Ensure(ctx context.Context, externalScopeID, kind, organizationID, name string) (core.ScopeBinding, error) {
	externalScopeID, kind, err := normalizeRef(externalScopeID, kind)
	if err != nil {
		return core.ScopeBinding{}, err
	}
	if existing, resolveErr := r.Resolve(ctx, externalScopeID, kind); resolveErr == nil {
		if name != "" {
			existing.ProjectName = strings.TrimSpace(name)
		}
		if organizationID != "" {
			existing.OrganizationID = strings.TrimSpace(organizationID)
		}
		existing.Status = "active"
		existing.UpdatedAt = time.Now()
		if err := r.persist(ctx, existing); err != nil {
			return core.ScopeBinding{}, err
		}
		return existing, nil
	} else if !errors.Is(resolveErr, ErrNotFound) && !strings.Contains(resolveErr.Error(), ErrNotFound.Error()) {
		return core.ScopeBinding{}, resolveErr
	}
	if r.scopeRoot == "" {
		return core.ScopeBinding{}, fmt.Errorf("%w: no scope_root configured", ErrNotConfigured)
	}
	projectID := derivedProjectID(r.provider, externalScopeID)
	rootPath := filepath.Join(r.scopeRoot, projectID)
	if !pathWithin(r.scopeRoot, rootPath) {
		return core.ScopeBinding{}, ErrPathOutsideRoot
	}
	projectName := strings.TrimSpace(name)
	if projectName == "" {
		projectName = externalScopeID
	}
	now := time.Now()
	binding := core.ScopeBinding{
		Provider:        r.provider,
		ExternalScopeID: externalScopeID,
		Kind:            kind,
		OrganizationID:  strings.TrimSpace(organizationID),
		ProjectID:       projectID,
		ProjectName:     projectName,
		RootPath:        rootPath,
		Status:          "active",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return core.ScopeBinding{}, fmt.Errorf("create scope root: %w", err)
	}
	if err := r.persist(ctx, binding); err != nil {
		return core.ScopeBinding{}, err
	}
	return binding, nil
}

func (r *Resolver) Register(ctx context.Context, binding core.ScopeBinding) error {
	binding = normalizeBinding(binding, r.provider)
	if err := validateBinding(binding); err != nil {
		return err
	}
	return r.persist(ctx, binding)
}

func (r *Resolver) persist(ctx context.Context, binding core.ScopeBinding) error {
	if err := validateBinding(binding); err != nil {
		return err
	}
	if r.store != nil {
		if err := r.store.UpsertProject(ctx, core.Project{
			ID: binding.ProjectID, Name: binding.ProjectName, RootPath: binding.RootPath,
			CreatedAt: binding.CreatedAt, UpdatedAt: binding.UpdatedAt,
		}); err != nil {
			return err
		}
		if err := r.store.UpsertScopeBinding(ctx, binding); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.bindings[binding.ExternalScopeID] = binding
	r.mu.Unlock()
	return nil
}

func normalizeBinding(binding core.ScopeBinding, provider string) core.ScopeBinding {
	if strings.TrimSpace(binding.Provider) == "" {
		binding.Provider = provider
	}
	binding.Provider = strings.TrimSpace(binding.Provider)
	binding.ExternalScopeID = strings.TrimSpace(binding.ExternalScopeID)
	binding.Kind = strings.TrimSpace(binding.Kind)
	if binding.Kind == "" {
		binding.Kind = "project"
	}
	binding.OrganizationID = strings.TrimSpace(binding.OrganizationID)
	binding.ProjectID = strings.TrimSpace(binding.ProjectID)
	binding.ProjectName = strings.TrimSpace(binding.ProjectName)
	binding.RootPath = strings.TrimSpace(binding.RootPath)
	binding.Status = strings.TrimSpace(binding.Status)
	if binding.Status == "" {
		binding.Status = "active"
	}
	return binding
}

func normalizeRef(externalScopeID, kind string) (string, string, error) {
	externalScopeID = strings.TrimSpace(externalScopeID)
	if externalScopeID == "" || len(externalScopeID) > 512 || strings.ContainsAny(externalScopeID, "/\\\x00") {
		return "", "", fmt.Errorf("%w: external scope id must be a non-empty path-safe identifier", ErrInvalid)
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "project"
	}
	if len(kind) > 64 || strings.ContainsAny(kind, "/\\\x00") {
		return "", "", fmt.Errorf("%w: invalid scope kind", ErrInvalid)
	}
	return externalScopeID, kind, nil
}

func validateBinding(binding core.ScopeBinding) error {
	if binding.Provider == "" || binding.ExternalScopeID == "" || binding.Kind == "" || binding.ProjectID == "" || binding.RootPath == "" {
		return fmt.Errorf("%w: incomplete scope binding", ErrInvalid)
	}
	if strings.ContainsAny(binding.Provider+binding.ExternalScopeID+binding.Kind+binding.ProjectID, "/\\\x00") {
		return fmt.Errorf("%w: unsafe scope binding identifier", ErrInvalid)
	}
	return nil
}

func validateResolved(binding core.ScopeBinding) (core.ScopeBinding, error) {
	if err := validateBinding(binding); err != nil {
		return core.ScopeBinding{}, err
	}
	if binding.Status == "archived" {
		return core.ScopeBinding{}, fmt.Errorf("%w: scope is archived", ErrNotFound)
	}
	return binding, nil
}

func derivedProjectID(provider, externalScopeID string) string {
	hash := sha256.Sum256([]byte(provider + "\x00" + externalScopeID))
	return "qm-" + hex.EncodeToString(hash[:])[:24]
}

func pathWithin(root, candidate string) bool {
	rootAbs, rootErr := filepath.Abs(root)
	candidateAbs, candidateErr := filepath.Abs(candidate)
	if rootErr != nil || candidateErr != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidateAbs))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

func isNoRows(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no rows"))
}
