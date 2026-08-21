package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestLocalInboundMaterializerWritesWorkspaceAndBuildsVisionInput(t *testing.T) {
	root := t.TempDir()
	transfer := t.TempDir()
	png := []byte("small-png-placeholder")
	blobID, _, err := data.PutLocalTransferBlob(transfer, strings.NewReader(string(png)), "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	materializer, err := NewLocalInboundMaterializer(root, transfer)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]IncomingAttachment{{Name: "../photo.png", MIMEType: "image/png; charset=binary", SizeBytes: int64(len(png)), BlobID: blobID, Author: "Alice"}})
	result, err := materializer.Materialize(context.Background(), "personal:alice", raw)
	if err != nil {
		t.Fatal(err)
	}
	var metas []AttachmentMeta
	if json.Unmarshal(result.Attachments, &metas) != nil || len(metas) != 1 || metas[0].Name != "photo.png" || metas[0].Direction != "in" {
		t.Fatalf("attachments=%s", result.Attachments)
	}
	if len(result.Images) != 1 || result.Images[0].DataBase64 != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("images=%#v", result.Images)
	}
	path, err := secureWorkspacePath(root, "personal:alice", "inbox/photo.png", false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(png) {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
	if !strings.Contains(result.Environment, "available in ./inbox/") || !strings.Contains(result.Environment, "shared by Alice") {
		t.Fatalf("environment=%q", result.Environment)
	}
}

func TestLocalInboundMaterializerDoesNotFollowWorkspaceSymlink(t *testing.T) {
	root := t.TempDir()
	transfer := t.TempDir()
	blobID, _, err := data.PutLocalTransferBlob(transfer, strings.NewReader("secret"), "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, safeWorkspaceKey("personal:alice"))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "inbox")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	raw, _ := json.Marshal([]IncomingAttachment{{Name: "escape.txt", MIMEType: "text/plain", BlobID: blobID}})
	materializer, _ := NewLocalInboundMaterializer(root, transfer)
	if _, err := materializer.Materialize(context.Background(), "personal:alice", raw); err == nil {
		t.Fatal("expected symlink confinement error")
	}
}
