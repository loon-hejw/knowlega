package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

const openCodeVersion = "1.17.18"

//go:embed opencode_bridge_plugin.mjs
var openCodeBridgePlugin []byte

type openCodeProcessRuntime struct {
	client      *http.Client
	baseURL     string
	directory   string
	secret      string
	definitions []ToolDefinition

	mu     sync.RWMutex
	states map[string]*OpenCodeBridgeState

	bridge         *http.Server
	bridgeListener net.Listener
	command        *exec.Cmd
	processDone    chan error
	eventCancel    context.CancelFunc
	closeOnce      sync.Once
}

func NewOpenCodeProcessRuntime(ctx context.Context, models config.ModelsConfig, harness config.ModelHarnessConfig, definitions []ToolDefinition) (OpenCodeRuntime, error) {
	provider, ok := models.Provider(harness.Provider)
	if !ok {
		return nil, fmt.Errorf("opencode provider %s is not configured", harness.Provider)
	}
	binary, err := findOpenCodeBinary(harness.Runtime.BinaryPath)
	if err != nil {
		return nil, err
	}
	jail, err := os.MkdirTemp("", "qm-opencode-")
	if err != nil {
		return nil, err
	}
	plugin := filepath.Join(jail, "opencode-bridge-plugin.mjs")
	if err := os.WriteFile(plugin, openCodeBridgePlugin, 0600); err != nil {
		_ = os.RemoveAll(jail)
		return nil, err
	}
	runtime := &openCodeProcessRuntime{
		client:    &http.Client{},
		directory: jail, secret: randomOpenCodeSecret(), definitions: append([]ToolDefinition(nil), definitions...),
		states: map[string]*OpenCodeBridgeState{}, processDone: make(chan error, 1),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(jail)
		return nil, err
	}
	runtime.bridgeListener = listener
	runtime.bridge = &http.Server{Handler: runtime.bridgeHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = runtime.bridge.Serve(listener) }()
	port, err := reserveOpenCodePort()
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	configuration := openCodeConfiguration(provider, definitions, plugin)
	encodedConfig, _ := json.Marshal(configuration)
	command := openCodeExecCommand(binary, "serve", "--hostname=127.0.0.1", fmt.Sprintf("--port=%d", port), "--log-level=ERROR")
	command.Dir = jail
	command.Env = []string{
		"PATH=" + filepath.Dir(binary) + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=" + jail, "TMPDIR=" + jail,
		"XDG_CONFIG_HOME=" + filepath.Join(jail, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(jail, ".data"),
		"XDG_CACHE_HOME=" + filepath.Join(jail, ".cache"),
		"OPENCODE_CONFIG_CONTENT=" + string(encodedConfig),
		"OPENCODE_BRIDGE_URL=http://" + listener.Addr().String(),
		"OPENCODE_BRIDGE_SECRET=" + runtime.secret,
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	runtime.command = command
	go func() { runtime.processDone <- command.Wait() }()
	startup := time.Duration(harness.Runtime.StartupTimeoutSeconds) * time.Second
	if startup <= 0 {
		startup = 90 * time.Second
	}
	serverURL, err := waitForOpenCodeServer(ctx, stdout, stderr, runtime.processDone, startup)
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	runtime.baseURL = strings.TrimRight(serverURL, "/")
	eventCtx, eventCancel := context.WithCancel(context.Background())
	runtime.eventCancel = eventCancel
	go runtime.consumeEvents(eventCtx)
	return runtime, nil
}

func (r *openCodeProcessRuntime) Register(sessionID string, state *OpenCodeBridgeState) {
	r.mu.Lock()
	r.states[sessionID] = state
	r.mu.Unlock()
}

func (r *openCodeProcessRuntime) Unregister(sessionID string) {
	r.mu.Lock()
	delete(r.states, sessionID)
	r.mu.Unlock()
}

func (r *openCodeProcessRuntime) CreateSession(ctx context.Context, title string) (string, error) {
	var response struct {
		ID string `json:"id"`
	}
	if err := r.request(ctx, http.MethodPost, "/session", map[string]string{"title": title}, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", errors.New("OpenCode session creation returned no id")
	}
	return response.ID, nil
}

func (r *openCodeProcessRuntime) Prompt(ctx context.Context, sessionID string, prompt OpenCodePrompt) (OpenCodeMessage, error) {
	body := openCodePromptBody(prompt)
	var response OpenCodeMessage
	err := r.request(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", body, &response)
	return response, err
}

func (r *openCodeProcessRuntime) PromptAsync(ctx context.Context, sessionID string, prompt OpenCodePrompt) error {
	return r.request(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/prompt_async", openCodePromptBody(prompt), nil)
}

func (r *openCodeProcessRuntime) WaitIdle(ctx context.Context, sessionID string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var statuses map[string]struct {
			Type string `json:"type"`
		}
		if err := r.request(ctx, http.MethodGet, "/session/status", nil, &statuses); err != nil {
			return err
		}
		status, exists := statuses[sessionID]
		if !exists || status.Type == "idle" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *openCodeProcessRuntime) Messages(ctx context.Context, sessionID string) ([]OpenCodeMessage, error) {
	var response []OpenCodeMessage
	err := r.request(ctx, http.MethodGet, "/session/"+url.PathEscape(sessionID)+"/message", nil, &response)
	return response, err
}

func (r *openCodeProcessRuntime) Abort(ctx context.Context, sessionID string) error {
	return r.request(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/abort", nil, nil)
}

func (r *openCodeProcessRuntime) DeleteSession(ctx context.Context, sessionID string) error {
	return r.request(ctx, http.MethodDelete, "/session/"+url.PathEscape(sessionID), nil, nil)
}

func (r *openCodeProcessRuntime) Close(ctx context.Context) error {
	var closeErr error
	r.closeOnce.Do(func() {
		if r.eventCancel != nil {
			r.eventCancel()
		}
		if r.command != nil && r.command.Process != nil {
			_ = r.command.Process.Signal(os.Interrupt)
			select {
			case <-r.processDone:
			case <-time.After(2 * time.Second):
				_ = r.command.Process.Kill()
			}
		}
		if r.bridge != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := r.bridge.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				closeErr = err
			}
		}
		if r.directory != "" {
			if err := os.RemoveAll(r.directory); closeErr == nil && err != nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func (r *openCodeProcessRuntime) request(ctx context.Context, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := r.baseURL + path
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := parsed.Query()
	query.Set("directory", r.directory)
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 20<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenCode %s %s failed: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if output != nil && len(bytes.TrimSpace(payload)) > 0 {
		return json.Unmarshal(payload, output)
	}
	return nil
}

func (r *openCodeProcessRuntime) bridgeHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !hmac.Equal([]byte(openCodeBearer(request)), []byte(r.secret)) {
			writeOpenCodeJSON(response, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if request.URL.Path == "/definitions" {
			definitions := make([]map[string]any, 0, len(r.definitions))
			for _, definition := range r.definitions {
				definitions = append(definitions, map[string]any{"name": definition.Name, "description": definition.Description, "parameters": rawJSONObject(definition.InputSchema)})
			}
			writeOpenCodeJSON(response, http.StatusOK, definitions)
			return
		}
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) != 3 || parts[0] != "session" {
			writeOpenCodeJSON(response, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		sessionID, err := url.PathUnescape(parts[1])
		if err != nil {
			writeOpenCodeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid session"})
			return
		}
		r.mu.RLock()
		state := r.states[sessionID]
		r.mu.RUnlock()
		if state == nil {
			writeOpenCodeJSON(response, http.StatusNotFound, map[string]string{"error": "inactive session"})
			return
		}
		switch parts[2] {
		case "context":
			writeOpenCodeJSON(response, http.StatusOK, map[string]any{
				"systemPrompt": state.SystemPrompt, "history": state.History,
				"proxyHeaders": map[string]string{"x-qm-session": sessionID, "x-qm-token": openCodeSessionToken(r.secret, sessionID)},
			})
		case "capture":
			payload, readErr := readOpenCodeBody(request, 16<<20)
			if readErr != nil {
				writeOpenCodeJSON(response, http.StatusBadRequest, map[string]string{"error": readErr.Error()})
				return
			}
			if state.Capture != nil {
				state.Capture(request.Context(), payload)
			}
			writeOpenCodeJSON(response, http.StatusOK, map[string]bool{"ok": true})
		case "tool":
			var input struct {
				Tool, CallID string
				Args         json.RawMessage
			}
			payload, readErr := readOpenCodeBody(request, 16<<20)
			if readErr != nil || json.Unmarshal(payload, &input) != nil {
				writeOpenCodeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid tool request"})
				return
			}
			if state.Execute == nil {
				writeOpenCodeJSON(response, http.StatusNotFound, map[string]string{"output": "[tool unavailable: " + input.Tool + "]"})
				return
			}
			result := state.Execute(request.Context(), ToolCall{ID: input.CallID, Name: input.Tool, Arguments: input.Args})
			writeOpenCodeJSON(response, http.StatusOK, map[string]any{"output": toolResultText(result), "terminate": result.Terminate || result.Silent})
		default:
			writeOpenCodeJSON(response, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	})
}

func (r *openCodeProcessRuntime) consumeEvents(ctx context.Context) {
	for ctx.Err() == nil {
		endpoint, _ := url.Parse(r.baseURL + "/global/event")
		query := endpoint.Query()
		query.Set("directory", r.directory)
		endpoint.RawQuery = query.Encode()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		response, err := r.client.Do(request)
		if err != nil {
			if !waitOpenCodeEventRetry(ctx) {
				return
			}
			continue
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
				continue
			}
			sessionID := openCodeEventSessionID(event)
			r.mu.RLock()
			state := r.states[sessionID]
			r.mu.RUnlock()
			if state != nil && state.OnEvent != nil {
				state.OnEvent(ctx, event)
			}
		}
		_ = response.Body.Close()
		if !waitOpenCodeEventRetry(ctx) {
			return
		}
	}
}

func openCodeConfiguration(provider config.ModelProviderConfig, definitions []ToolDefinition, pluginPath string) map[string]any {
	models := map[string]any{}
	for _, model := range provider.Models {
		definition := map[string]any{"name": model.Name}
		if model.ContextWindow > 0 || model.MaxTokens > 0 {
			definition["limit"] = map[string]int{"context": max(1, model.ContextWindow), "output": max(1, model.MaxTokens)}
		}
		models[model.ID] = definition
	}
	npm := "@ai-sdk/openai-compatible"
	if provider.Protocol == "anthropic" {
		npm = "@ai-sdk/anthropic"
	}
	enabled := map[string]bool{}
	for _, definition := range definitions {
		enabled[definition.Name] = true
	}
	disabled := map[string]bool{"read": false, "write": false, "bash": false, "edit": false, "apply_patch": false, "glob": false, "grep": false, "webfetch": false, "websearch": false, "codesearch": false, "patch": false, "question": false, "skill": false, "todowrite": false, "todoread": false}
	for name, value := range enabled {
		disabled[name] = value
	}
	tools := cloneBoolMap(enabled)
	tools["task"] = true
	return map[string]any{
		"plugin": []string{fileURL(pluginPath)}, "autoupdate": false, "share": "disabled", "snapshot": false, "lsp": false, "formatter": false, "instructions": []string{},
		"enabled_providers": []string{provider.ID},
		"provider":          map[string]any{provider.ID: map[string]any{"npm": npm, "name": provider.ID, "options": map[string]string{"baseURL": provider.BaseURL, "apiKey": provider.APIKey}, "models": models}},
		"tools":             disabled,
		"permission":        map[string]string{"read": "deny", "write": "deny", "edit": "deny", "apply_patch": "deny", "bash": "deny", "glob": "deny", "grep": "deny", "webfetch": "deny", "websearch": "deny", "external_directory": "deny", "doom_loop": "deny"},
		"agent": map[string]any{
			"qm":       map[string]any{"mode": "primary", "prompt": "", "tools": tools},
			"research": map[string]any{"mode": "subagent", "description": "Research a bounded question and report evidence.", "prompt": "Complete only the delegated research task.", "tools": tools},
			"code":     map[string]any{"mode": "subagent", "description": "Implement or inspect a bounded code task.", "prompt": "Complete only the delegated code task.", "tools": tools},
			"consult":  map[string]any{"mode": "subagent", "description": "Provide an independent expert analysis.", "prompt": "Complete only the delegated consultation.", "tools": tools},
		},
	}
}

func openCodePromptBody(prompt OpenCodePrompt) map[string]any {
	body := map[string]any{"model": map[string]string{"providerID": prompt.ModelProvider, "modelID": prompt.ModelID}, "agent": "qm", "parts": prompt.Parts}
	if prompt.System != "" {
		body["system"] = prompt.System
	}
	if prompt.Tools != nil {
		body["tools"] = prompt.Tools
	}
	return body
}

func findOpenCodeBinary(configured string) (string, error) {
	candidates := []string{strings.TrimSpace(configured)}
	candidates = append(candidates, findOpenCodeUpward("qm/node_modules/.bin/opencode")...)
	candidates = append(candidates, findOpenCodeUpward("node_modules/.bin/opencode")...)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err == nil {
			if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
				return absolute, nil
			}
		}
	}
	return "", errors.New("OpenCode binary not found; configure qm.models.harnesses[].runtime.binary_path")
}

func openCodeExecCommand(binary string, args ...string) *exec.Cmd {
	prefix := make([]byte, 4096)
	file, err := os.Open(binary)
	if err == nil {
		count, _ := file.Read(prefix)
		_ = file.Close()
		prefix = prefix[:count]
		if count > 0 && !bytes.HasPrefix(prefix, []byte("#!")) && utf8.Valid(prefix) && !bytes.ContainsRune(prefix, '\x00') {
			return exec.Command("/bin/sh", append([]string{binary}, args...)...)
		}
	}
	return exec.Command(binary, args...)
}

func findOpenCodeUpward(relative string) []string {
	directory, err := os.Getwd()
	if err != nil {
		return nil
	}
	result := []string{}
	for depth := 0; depth < 8; depth++ {
		result = append(result, filepath.Join(directory, relative))
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return result
}

func reserveOpenCodePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

func waitForOpenCodeServer(ctx context.Context, stdout, stderr io.Reader, done <-chan error, timeout time.Duration) (string, error) {
	lines := make(chan string, 64)
	read := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default:
			}
		}
	}
	go read(stdout)
	go read(stderr)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	pattern := regexp.MustCompile(`opencode server listening.*?on\s+(https?://\S+)`)
	diagnostics := []string{}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-done:
			return "", fmt.Errorf("OpenCode %s exited during startup: %v: %s", openCodeVersion, err, strings.Join(diagnostics, "\n"))
		case <-timer.C:
			return "", fmt.Errorf("OpenCode %s did not start within %s: %s", openCodeVersion, timeout, strings.Join(diagnostics, "\n"))
		case line := <-lines:
			diagnostics = append(diagnostics, line)
			if len(diagnostics) > 40 {
				diagnostics = diagnostics[len(diagnostics)-40:]
			}
			match := pattern.FindStringSubmatch(line)
			if len(match) == 2 {
				return match[1], nil
			}
		}
	}
}

func readOpenCodeBody(request *http.Request, maxBytes int64) (json.RawMessage, error) {
	payload, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, errors.New("request body too large")
	}
	if !json.Valid(payload) {
		return nil, errors.New("invalid JSON")
	}
	return payload, nil
}

func writeOpenCodeJSON(response http.ResponseWriter, status int, value any) {
	payload, _ := json.Marshal(value)
	response.Header().Set("content-type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write(payload)
}

func randomOpenCodeSecret() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func openCodeBearer(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("authorization"))
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}

func openCodeSessionToken(secret, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func openCodeEventSessionID(event map[string]any) string {
	payload, _ := event["payload"].(map[string]any)
	properties, _ := payload["properties"].(map[string]any)
	part, _ := properties["part"].(map[string]any)
	return fmt.Sprint(part["sessionID"])
}

func waitOpenCodeEventRetry(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func fileURL(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func cloneBoolMap(source map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
