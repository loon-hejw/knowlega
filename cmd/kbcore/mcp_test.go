package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hejw/knowledge-core/internal/wiki"
)

func TestMCPToolsListAndWriteConfirmGuard(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	server := mcpServer{projectPath: root, projectID: "project"}
	resp := server.handle(mcpRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"})
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	data, _ := json.Marshal(resp.Result)
	if !containsAll(string(data), "kbcore_read_file", "kbcore_delete_source") {
		t.Fatalf("unexpected tools: %s", data)
	}
	params, _ := json.Marshal(mcpToolCall{
		Name:      "kbcore_rescan_sources",
		Arguments: map[string]any{},
	})
	resp = server.handle(mcpRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/call", Params: params})
	if resp.Error == nil {
		t.Fatalf("expected confirm guard error, got %+v", resp)
	}
}

func containsAll(value string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(value, want) {
			return false
		}
	}
	return true
}
