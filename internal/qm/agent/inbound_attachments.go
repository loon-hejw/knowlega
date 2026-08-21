package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

const (
	maxInboundAttachments = 10
	maxVisionImageBytes   = 5_000_000
	maxInboundBytes       = 1_000_000_000
)

type IncomingAttachment struct {
	Name      string `json:"name"`
	MIMEType  string `json:"mimetype"`
	SizeBytes int64  `json:"sizeBytes"`
	BlobID    string `json:"blobId"`
	SourceID  string `json:"sourceId,omitempty"`
	Author    string `json:"author,omitempty"`
}

type AttachmentMeta struct {
	Name       string `json:"name"`
	MIMEType   string `json:"mimetype"`
	SizeBytes  int64  `json:"sizeBytes"`
	Direction  string `json:"direction"`
	Author     string `json:"author,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
}

type MaterializedInbound struct {
	Attachments json.RawMessage
	Images      []Image
	Environment string
	Unavailable []string
	TooMany     []string
}

type InboundMaterializer interface {
	Materialize(context.Context, string, json.RawMessage) (MaterializedInbound, error)
}

type LocalInboundMaterializer struct {
	workspaceRoot string
	transferDir   string
}

func NewLocalInboundMaterializer(workspaceRoot, transferDir string) (*LocalInboundMaterializer, error) {
	root, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		if err == nil {
			err = errors.New("agent workspace root is required")
		}
		return nil, err
	}
	transfer := strings.TrimSpace(transferDir)
	if transfer != "" {
		transfer, err = filepath.Abs(transfer)
		if err != nil {
			return nil, err
		}
	}
	return &LocalInboundMaterializer{workspaceRoot: root, transferDir: transfer}, nil
}

func (m *LocalInboundMaterializer) Materialize(ctx context.Context, workspaceKey string, raw json.RawMessage) (MaterializedInbound, error) {
	var incoming []IncomingAttachment
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return MaterializedInbound{}, fmt.Errorf("decode inbound attachments: %w", err)
	}
	metas := []AttachmentMeta{}
	images := []Image{}
	unavailable := []string{}
	tooMany := []string{}
	used := map[string]bool{}
	for _, attachment := range incoming {
		if err := ctx.Err(); err != nil {
			return MaterializedInbound{}, err
		}
		name := uniqueAttachmentName(safeAttachmentName(attachment.Name), used)
		if len(metas) >= maxInboundAttachments {
			tooMany = append(tooMany, name)
			continue
		}
		if m.transferDir == "" {
			unavailable = append(unavailable, name)
			continue
		}
		opened, size, err := data.OpenLocalTransferBlob(m.transferDir, attachment.BlobID)
		if err != nil {
			return MaterializedInbound{}, err
		}
		if opened == nil || size < 0 || size > maxInboundBytes {
			unavailable = append(unavailable, name)
			continue
		}
		path, err := secureWorkspacePath(m.workspaceRoot, workspaceKey, filepath.ToSlash(filepath.Join("inbox", name)), true)
		if err != nil {
			_ = opened.Close()
			return MaterializedInbound{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			_ = opened.Close()
			return MaterializedInbound{}, err
		}
		part := path + ".part"
		out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			_ = opened.Close()
			return MaterializedInbound{}, err
		}
		_, copyErr := io.Copy(out, io.LimitReader(opened, maxInboundBytes+1))
		closeOutErr := out.Close()
		closeInErr := opened.Close()
		if copyErr != nil || closeOutErr != nil || closeInErr != nil {
			_ = os.Remove(part)
			return MaterializedInbound{}, errors.Join(copyErr, closeOutErr, closeInErr)
		}
		if err := os.Rename(part, path); err != nil {
			_ = os.Remove(part)
			return MaterializedInbound{}, err
		}
		mime := baseAttachmentMIME(attachment.MIMEType, name)
		metas = append(metas, AttachmentMeta{Name: name, MIMEType: mime, SizeBytes: size, Direction: "in", Author: strings.TrimSpace(attachment.Author)})
		if isVisionMIME(mime) && size > 0 && size <= maxVisionImageBytes {
			bytes, readErr := os.ReadFile(path)
			if readErr != nil {
				return MaterializedInbound{}, readErr
			}
			images = append(images, Image{MIMEType: mime, DataBase64: base64.StdEncoding.EncodeToString(bytes)})
		}
	}
	encoded, err := json.Marshal(metas)
	if err != nil {
		return MaterializedInbound{}, err
	}
	environment := inboundEnvironment(metas, tooMany, unavailable)
	return MaterializedInbound{Attachments: encoded, Images: images, Environment: environment, Unavailable: unavailable, TooMany: tooMany}, nil
}

func hasJSONArrayItems(raw json.RawMessage) bool {
	var values []json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &values) == nil && len(values) > 0
}

func readOnlyAttachmentEnvironment(raw json.RawMessage) string {
	var incoming []IncomingAttachment
	if json.Unmarshal(raw, &incoming) != nil || len(incoming) == 0 {
		return ""
	}
	issues := make([]string, 0, len(incoming))
	for _, attachment := range incoming {
		issues = append(issues, safeAttachmentName(attachment.Name)+" — unavailable in Strict posture")
	}
	return environmentNote("Files received this turn that did not reach ./inbox/: " + strings.Join(issues, "; "))
}

func inboundEnvironment(metas []AttachmentMeta, tooMany, unavailable []string) string {
	sections := []string{}
	if len(metas) > 0 {
		lines := make([]string, 0, len(metas))
		for _, meta := range metas {
			line := fmt.Sprintf("- inbox/%s (%s, %d bytes)", meta.Name, meta.MIMEType, meta.SizeBytes)
			if meta.Author != "" {
				line += " — shared by " + meta.Author
			}
			lines = append(lines, line)
		}
		noun := "files"
		if len(metas) == 1 {
			noun = "file"
		}
		lead := fmt.Sprintf("The user shared %d %s, available in ./inbox/:", len(metas), noun)
		if hasAttachmentAuthor(metas) {
			lead = fmt.Sprintf("%d %s shared in this conversation, available in ./inbox/:", len(metas), noun)
		}
		sections = append(sections, lead+"\n"+strings.Join(lines, "\n"))
	}
	issues := []string{}
	if len(tooMany) > 0 {
		issues = append(issues, fmt.Sprintf("%s — too many files in one message (only the first %d were taken)", strings.Join(tooMany, ", "), maxInboundAttachments))
	}
	if len(unavailable) > 0 {
		issues = append(issues, strings.Join(unavailable, ", ")+" — no longer available (the upload may have expired)")
	}
	if len(issues) > 0 {
		sections = append(sections, "Files received this turn that did not reach ./inbox/: "+strings.Join(issues, "; "))
	}
	return environmentNote(strings.Join(sections, "\n\n"))
}

func environmentNote(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "<environment>\n" + strings.TrimSpace(value) + "\n</environment>"
}

func joinEnvironment(values ...string) string {
	parts := []string{}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	return strings.Join(parts, "\n\n")
}

func safeAttachmentName(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	name := filepath.Base(value)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

func uniqueAttachmentName(name string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for index := 2; ; index++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, index, ext)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

func baseAttachmentMIME(value, name string) string {
	if index := strings.Index(value, ";"); index >= 0 {
		value = value[:index]
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "" {
		return value
	}
	extensions := map[string]string{".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv", ".json": "application/json", ".html": "text/html", ".xml": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml", ".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml", ".zip": "application/zip"}
	return firstNonEmpty(extensions[strings.ToLower(filepath.Ext(name))], "application/octet-stream")
}

func isVisionMIME(value string) bool {
	return value == "image/png" || value == "image/jpeg" || value == "image/gif" || value == "image/webp"
}

func hasAttachmentAuthor(metas []AttachmentMeta) bool {
	for _, meta := range metas {
		if meta.Author != "" {
			return true
		}
	}
	return false
}
