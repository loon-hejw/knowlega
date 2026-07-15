package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/hejw/knowledge-core/internal/service"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type mcpServer struct {
	projectPath string
	projectID   string
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	project := fs.String("project", runtimeConfig.Project.Path, "project path")
	projectID := fs.String("project-id", runtimeConfig.Database.ProjectID, "project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*project) == "" {
		return fmt.Errorf("mcp requires --project")
	}
	server := mcpServer{projectPath: *project, projectID: *projectID}
	return server.serve(os.Stdin, os.Stdout)
}

func (s mcpServer) serve(in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		data, err := readMCPMessage(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var req mcpRequest
		if err := json.Unmarshal(data, &req); err != nil {
			if writeErr := writeMCPMessage(out, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: err.Error()}}); writeErr != nil {
				return writeErr
			}
			continue
		}
		resp := s.handle(req)
		if req.ID == nil && resp.Error == nil {
			continue
		}
		if err := writeMCPMessage(out, resp); err != nil {
			return err
		}
	}
}

func (s mcpServer) handle(req mcpRequest) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]any{
				"name":    "kbcore",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{"tools": map[string]any{}},
		}
	case "notifications/initialized":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools()}
	case "tools/call":
		var call mcpToolCall
		if err := json.Unmarshal(req.Params, &call); err != nil {
			resp.Error = &mcpError{Code: -32602, Message: err.Error()}
			return resp
		}
		result, err := s.callTool(call)
		if err != nil {
			resp.Error = &mcpError{Code: -32000, Message: err.Error()}
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]string{{"type": "text", "text": result}}}
	default:
		resp.Error = &mcpError{Code: -32601, Message: "method not found"}
	}
	return resp
}

func (s mcpServer) callTool(call mcpToolCall) (string, error) {
	args := call.Arguments
	switch call.Name {
	case "kbcore_list_files":
		files, err := service.ListProjectFiles(s.projectPath)
		return jsonText(files), err
	case "kbcore_read_file":
		content, err := service.ReadProjectFile(s.projectPath, stringArg(args, "path"))
		return content, err
	case "kbcore_search":
		limit := intArg(args, "limit", 10)
		results, err := service.QueryWiki(s.projectPath, stringArg(args, "query"), limit)
		return jsonText(results), err
	case "kbcore_graph":
		graph, err := service.WikiGraphForAPI(s.projectPath)
		return jsonText(graph), err
	case "kbcore_list_reviews":
		items, err := wiki.ScanReviewItems(wiki.ScanOptions{ProjectPath: s.projectPath, ProjectID: s.projectID})
		if err != nil {
			return "", err
		}
		status := stringArg(args, "status")
		if status != "" && status != "all" {
			filtered := items[:0]
			for _, item := range items {
				if item.Status == status {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		return jsonText(items), nil
	case "kbcore_rescan_sources":
		if !boolArg(args, "confirm", false) {
			return "", fmt.Errorf("kbcore_rescan_sources requires confirm=true")
		}
		result, err := service.ScanRawSources(service.QueueIngestOptions{ProjectPath: s.projectPath})
		return jsonText(result), err
	case "kbcore_resolve_reviews":
		if !boolArg(args, "confirm", false) {
			return "", fmt.Errorf("kbcore_resolve_reviews requires confirm=true")
		}
		ids := stringSliceArg(args, "ids")
		status := stringArg(args, "status")
		if status == "" {
			status = "resolved"
		}
		action := stringArg(args, "action")
		if action == "" {
			action = "mcp"
		}
		resolved, err := service.UpdateReviewItemsStatus(s.projectPath, s.projectID, ids, status, action)
		if err != nil {
			return "", err
		}
		return jsonText(resolved), nil
	case "kbcore_delete_source":
		dryRun := boolArg(args, "dry_run", false)
		if !dryRun && !boolArg(args, "confirm", false) {
			return "", fmt.Errorf("kbcore_delete_source requires dry_run=true or confirm=true")
		}
		result, err := service.DeleteSource(service.DeleteSourceOptions{
			ProjectPath: s.projectPath,
			ProjectID:   s.projectID,
			SourcePath:  stringArg(args, "source_path"),
			DeleteRaw:   boolArg(args, "delete_raw", false),
			DryRun:      dryRun,
		})
		return jsonText(result), err
	default:
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
}

func mcpTools() []map[string]any {
	tool := func(name, description string, props map[string]any, required []string) map[string]any {
		return map[string]any{
			"name":        name,
			"description": description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		}
	}
	stringSchema := map[string]any{"type": "string"}
	boolSchema := map[string]any{"type": "boolean"}
	intSchema := map[string]any{"type": "integer"}
	return []map[string]any{
		tool("kbcore_list_files", "List wiki/raw/project files.", map[string]any{}, nil),
		tool("kbcore_read_file", "Read a project file.", map[string]any{"path": stringSchema}, []string{"path"}),
		tool("kbcore_search", "Search wiki and raw sources.", map[string]any{"query": stringSchema, "limit": intSchema}, []string{"query"}),
		tool("kbcore_graph", "Return wikilink graph nodes and edges.", map[string]any{}, nil),
		tool("kbcore_list_reviews", "List review tasks.", map[string]any{"status": stringSchema}, nil),
		tool("kbcore_rescan_sources", "Rescan raw/sources.", map[string]any{"confirm": boolSchema}, []string{"confirm"}),
		tool("kbcore_resolve_reviews", "Resolve review tasks.", map[string]any{"ids": map[string]any{"type": "array", "items": stringSchema}, "status": stringSchema, "action": stringSchema, "confirm": boolSchema}, []string{"ids", "confirm"}),
		tool("kbcore_delete_source", "Dry-run or delete a source with cleanup.", map[string]any{"source_path": stringSchema, "dry_run": boolSchema, "delete_raw": boolSchema, "confirm": boolSchema}, []string{"source_path"}),
	}
}

func readMCPMessage(reader *bufio.Reader) ([]byte, error) {
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			parsed, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, err
			}
			contentLength = parsed
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	data := make([]byte, contentLength)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func writeMCPMessage(out io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

func jsonText(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func stringArg(args map[string]any, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
}

func intArg(args map[string]any, key string, fallback int) int {
	switch value := args[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return fallback
	}
}

func boolArg(args map[string]any, key string, fallback bool) bool {
	if value, ok := args[key].(bool); ok {
		return value
	}
	return fallback
}

func stringSliceArg(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if value, ok := item.(string); ok {
			out = append(out, value)
		}
	}
	return out
}
