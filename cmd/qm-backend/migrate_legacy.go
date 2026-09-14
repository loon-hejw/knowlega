package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	knowledgecompiler "github.com/loon-hejw/knowlega/internal/agent/knowlega/compiler"
	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/qm/biz"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type legacyKnowledgeImportOptions struct {
	Config      config.Config
	Postgres    *data.Postgres
	Agent       *knowlega.Agent
	SourcePath  string
	ProjectName string
	OwnerID     string
}

type legacyKnowledgeImportResult struct {
	ProjectID string
	ScopeID   string
	Files     int
	WikiPages int64
	Sources   int64
}

type legacyImportMarker struct {
	Version     int    `json:"version"`
	SourcePath  string `json:"source_path"`
	ImportedAt  string `json:"imported_at"`
	ConvergedAt string `json:"converged_at,omitempty"`
}

func importLegacyKnowledgeProject(ctx context.Context, opts legacyKnowledgeImportOptions) (legacyKnowledgeImportResult, error) {
	if opts.Postgres == nil || opts.Agent == nil {
		return legacyKnowledgeImportResult{}, errors.New("PostgreSQL and knowledge agent are required")
	}
	projectName := strings.TrimSpace(opts.ProjectName)
	ownerID := strings.TrimSpace(opts.OwnerID)
	if projectName == "" || ownerID == "" {
		return legacyKnowledgeImportResult{}, errors.New("import-project-name and import-project-owner are required")
	}
	sourcePath, err := filepath.Abs(strings.TrimSpace(opts.SourcePath))
	if err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("resolve legacy project path: %w", err)
	}
	if err := validateLegacyKnowledgeProject(sourcePath); err != nil {
		return legacyKnowledgeImportResult{}, err
	}

	projects := data.NewProjectRepository(opts.Postgres, opts.Config.QM.OrgID)
	project, err := findOrCreateImportProject(ctx, projects, ownerID, projectName)
	if err != nil {
		return legacyKnowledgeImportResult{}, err
	}
	scopeID := biz.ProjectScopeID(project.ID)
	ref := knowlega.ScopeRef{OrgID: opts.Config.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", Name: project.Name}
	status, err := opts.Agent.EnsureScope(ctx, ref)
	if err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("ensure project knowledge scope: %w", err)
	}
	if _, err := data.NewKnowledgeScopeRepository(opts.Postgres).Ensure(ctx, data.KnowledgeScope{
		OrgID: opts.Config.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", ProjectID: status.ProjectID,
		ProjectName: project.Name, RootPath: status.ProjectPath, Status: status.State,
	}); err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("persist project knowledge scope: %w", err)
	}
	if err := copyLegacyKnowledgeProject(sourcePath, status.ProjectPath); err != nil {
		return legacyKnowledgeImportResult{}, err
	}
	if err := convergeLegacyKnowledgeProject(sourcePath, status.ProjectPath); err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("converge imported wiki: %w", err)
	}

	manifest, err := manifestfile.Load(status.ProjectPath)
	if err != nil {
		return legacyKnowledgeImportResult{}, err
	}
	keys := make([]string, 0, len(manifest.Sources))
	for key := range manifest.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	files := data.NewFileArtifactRepository(opts.Postgres)
	memberships := data.NewProjectFileMembershipRepository(opts.Postgres)
	acl := data.NewACLRepository(opts.Postgres)
	ownerScope := "personal:" + ownerID
	for _, key := range keys {
		entry := manifest.Sources[key]
		contentPath, err := legacyManifestContentPath(status.ProjectPath, entry)
		if err != nil {
			return legacyKnowledgeImportResult{}, fmt.Errorf("source %q: %w", key, err)
		}
		name := legacySourceName(key, entry)
		artifact, err := importLegacyQMFile(ctx, files, acl, opts.Config.QM.FileStore.LocalDir, project.ID, scopeID, ownerScope, ownerID, name, contentPath)
		if err != nil {
			return legacyKnowledgeImportResult{}, fmt.Errorf("import QM file %q: %w", name, err)
		}
		membership, err := memberships.PutQueued(ctx, data.ProjectFileMembership{
			ProjectID: project.ID, ProjectScopeID: scopeID, FileID: artifact.ID,
			KnowledgeProjectID: status.ProjectID, SourceSHA256: artifact.SHA256,
		})
		if err != nil {
			return legacyKnowledgeImportResult{}, err
		}
		if err := opts.Agent.BindQMFile(ref, key, artifact.ID, project.ID, artifact.SHA256); err != nil {
			return legacyKnowledgeImportResult{}, fmt.Errorf("bind QM file %q: %w", name, err)
		}
		rawPath := filepath.ToSlash(firstLegacyPath(entry.ContentPath, entry.OriginalRawPath, entry.RawPath))
		if err := memberships.SetState(ctx, project.ID, artifact.ID, "ready", &rawPath, membership.QueueTaskID, len(entry.Files), nil); err != nil {
			return legacyKnowledgeImportResult{}, err
		}
	}
	if err := opts.Agent.SyncWiki(ctx, ref); err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("sync imported wiki to PostgreSQL: %w", err)
	}
	status, err = opts.Agent.RefreshScope(ctx, ref)
	if err != nil {
		return legacyKnowledgeImportResult{}, err
	}
	if _, err := data.NewKnowledgeScopeRepository(opts.Postgres).Ensure(ctx, data.KnowledgeScope{
		OrgID: opts.Config.QM.OrgID, ExternalScopeID: scopeID, Kind: "project", ProjectID: status.ProjectID,
		ProjectName: project.Name, RootPath: status.ProjectPath, Status: status.State,
	}); err != nil {
		return legacyKnowledgeImportResult{}, fmt.Errorf("refresh project knowledge scope: %w", err)
	}
	return legacyKnowledgeImportResult{ProjectID: project.ID, ScopeID: scopeID, Files: len(keys), WikiPages: status.WikiPageCount, Sources: status.SourceCount}, nil
}

func convergeLegacyKnowledgeProject(sourcePath, targetPath string) error {
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(targetPath, ".kbcore", "qm-legacy-import.json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	var marker legacyImportMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return fmt.Errorf("decode legacy import marker: %w", err)
	}
	if marker.SourcePath != sourcePath {
		return errors.New("knowledge scope was imported from a different legacy project")
	}
	if marker.Version >= 6 {
		return nil
	}
	if _, err := knowledgecompiler.RestoreQuarantinedSourcePages(targetPath); err != nil {
		return err
	}
	if _, err := knowledgecompiler.ConvergeWikiArtifacts(targetPath); err != nil {
		return err
	}
	marker.Version = 6
	marker.ConvergedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err = json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath, append(raw, '\n'), 0o644)
}

func findOrCreateImportProject(ctx context.Context, projects *data.ProjectRepository, ownerID, name string) (*biz.Project, error) {
	owned, err := projects.FindOwnedByName(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if len(owned) > 1 {
		return nil, fmt.Errorf("multiple QM projects named %q are owned by %s", name, ownerID)
	}
	if len(owned) == 1 {
		return &owned[0], nil
	}
	items, err := projects.ListForMember(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	var match *biz.Project
	for i := range items {
		if items[i].Name != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("multiple QM projects named %q are owned by %s", name, ownerID)
		}
		copy := items[i]
		match = &copy
	}
	if match != nil {
		return match, nil
	}
	created, err := projects.Create(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, fmt.Errorf("QM principal %q is not an active internal member", ownerID)
	}
	return created, nil
}

func validateLegacyKnowledgeProject(sourcePath string) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("open legacy knowledge project: %w", err)
	}
	if !info.IsDir() {
		return errors.New("legacy knowledge project path must be a directory")
	}
	for _, required := range []string{"purpose.md", "schema.md", filepath.Join("wiki", "index.md"), filepath.Join(".kbcore", "source-manifest.json")} {
		if info, err := os.Stat(filepath.Join(sourcePath, required)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("legacy knowledge project is missing %s", filepath.ToSlash(required))
		}
	}
	manifest, err := manifestfile.Load(sourcePath)
	if err != nil {
		return err
	}
	if len(manifest.Sources) == 0 {
		return errors.New("legacy knowledge project has no source manifest entries")
	}
	return nil
}

func copyLegacyKnowledgeProject(sourcePath, targetPath string) error {
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	targetPath, err = filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	if sourcePath == targetPath {
		return errors.New("legacy source and target knowledge paths are identical")
	}
	markerPath := filepath.Join(targetPath, ".kbcore", "qm-legacy-import.json")
	if raw, readErr := os.ReadFile(markerPath); readErr == nil {
		var marker legacyImportMarker
		if json.Unmarshal(raw, &marker) == nil && marker.SourcePath == sourcePath {
			return nil
		}
		return errors.New("knowledge scope was already imported from a different legacy project")
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := requireEmptyKnowledgeTarget(targetPath); err != nil {
		return err
	}
	if err := filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourcePath, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("legacy project path escaped its root")
		}
		destination := filepath.Join(targetPath, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("legacy project contains unsupported non-regular file %s", rel)
		}
		mode := info.Mode().Perm()
		if strings.HasPrefix(filepath.ToSlash(rel), "raw/sources/") {
			mode = 0o444
		}
		return copyLegacyFile(path, destination, mode)
	}); err != nil {
		return fmt.Errorf("copy legacy knowledge project: %w", err)
	}
	marker := legacyImportMarker{Version: 1, SourcePath: sourcePath, ImportedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(markerPath, raw, 0o644)
}

func requireEmptyKnowledgeTarget(targetPath string) error {
	manifest, err := manifestfile.Load(targetPath)
	if err != nil {
		return err
	}
	if len(manifest.Sources) != 0 {
		return errors.New("target knowledge scope already contains sources")
	}
	allowed := map[string]bool{"index.md": true, "overview.md": true, "log.md": true, "reviews.md": true}
	return filepath.WalkDir(filepath.Join(targetPath, "wiki"), func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(filepath.Join(targetPath, "wiki"), path)
		if err != nil {
			return err
		}
		if !allowed[filepath.ToSlash(rel)] {
			return fmt.Errorf("target knowledge scope already contains wiki page %s", filepath.ToSlash(rel))
		}
		return nil
	})
}

func copyLegacyFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".legacy-import-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func legacyManifestContentPath(projectPath string, entry manifestfile.Entry) (string, error) {
	rel := firstLegacyPath(entry.ContentPath, entry.OriginalRawPath, entry.RawPath)
	if rel == "" {
		return "", errors.New("manifest entry has no content path")
	}
	root, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", errors.New("manifest content path escapes the knowledge project")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("manifest content path is not a regular file")
	}
	return path, nil
}

func firstLegacyPath(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func legacySourceName(key string, entry manifestfile.Entry) string {
	name := filepath.Base(firstLegacyPath(entry.OriginalPath, key, entry.ContentPath, entry.RawPath))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "source.txt"
	}
	return strings.ReplaceAll(name, "..", "_")
}

func importLegacyQMFile(ctx context.Context, files *data.FileArtifactRepository, acl *data.ACLRepository, localDir, projectID, scopeID, ownerScope, ownerID, name, sourcePath string) (*data.FileArtifact, error) {
	input, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	blobKey, size, err := data.PutLocalFileBlob(localDir, input, 1_000_000_000)
	_ = input.Close()
	if err != nil {
		return nil, err
	}
	sha := strings.TrimPrefix(blobKey, "files/")
	sum := sha256.Sum256([]byte("qm-legacy-file\x00" + projectID + "\x00" + name + "\x00" + sha))
	id := hex.EncodeToString(sum[:16])
	path := "artifacts/" + id + "/" + name
	existing, err := files.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.SHA256 != sha || existing.Name != name || existing.Path != path {
			return nil, fmt.Errorf("stable file id %s already has different metadata", id)
		}
		return existing, acl.Put(ctx, data.Grant{OwnerScopeID: existing.OwnerScopeID, Path: existing.Path, GranteeScopeID: scopeID, Permission: "read", GrantedBy: ownerID})
	}
	now := time.Now().UnixMilli()
	createdInScope := scopeID
	blob := blobKey
	artifact, err := files.Create(ctx, data.FileArtifact{
		ID: id, OwnerScopeID: ownerScope, CreatedBy: ownerID, Name: name, Path: path,
		Mimetype: legacyMimetype(name), SizeBytes: size, BlobKey: &blob, SHA256: sha,
		Direction: "in", CreatedInScope: &createdInScope, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	if err := acl.Put(ctx, data.Grant{OwnerScopeID: ownerScope, Path: path, GranteeScopeID: scopeID, Permission: "read", GrantedBy: ownerID}); err != nil {
		return nil, err
	}
	return artifact, nil
}

func legacyMimetype(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		return "text/markdown"
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	default:
		return "text/plain"
	}
}
