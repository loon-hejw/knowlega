package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CodexRPC interface {
	Request(context.Context, string, any, any) error
	Notify(context.Context, string, any) error
	Close(context.Context) error
}

type CodexNotificationHandler func(context.Context, string, json.RawMessage)
type CodexRequestHandler func(context.Context, string, json.RawMessage) (any, error)

type codexRPCClient struct {
	command *exec.Cmd
	stdin   io.WriteCloser

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan codexRPCResponse
	closed  bool
	stderr  strings.Builder
	done    chan struct{}

	onNotification CodexNotificationHandler
	onRequest      CodexRequestHandler
}

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

type codexRPCResponse struct {
	result json.RawMessage
	err    error
}

func newCodexRPCClient(ctx context.Context, binary, directory string, environment []string, onNotification CodexNotificationHandler, onRequest CodexRequestHandler) (*codexRPCClient, error) {
	command := exec.CommandContext(ctx, binary, "app-server")
	command.Dir = directory
	command.Env = environment
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	client := &codexRPCClient{command: command, stdin: stdin, pending: map[int64]chan codexRPCResponse{}, done: make(chan struct{}), onNotification: onNotification, onRequest: onRequest}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go client.readStdout(stdout)
	go client.readStderr(stderr)
	go func() {
		err := command.Wait()
		client.failAll(fmt.Errorf("Codex app-server exited: %w%s", err, client.stderrSuffix()))
		close(client.done)
	}()
	return client, nil
}

func (c *codexRPCClient) Initialize(ctx context.Context) error {
	var ignored any
	if err := c.Request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "qm", "title": "QM", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, &ignored); err != nil {
		return err
	}
	return c.Notify(ctx, "initialized", nil)
}

func (c *codexRPCClient) Request(ctx context.Context, method string, params any, output any) error {
	if strings.TrimSpace(method) == "" {
		return errors.New("Codex RPC method is required")
	}
	id := c.nextID.Add(1)
	waiter := make(chan codexRPCResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("Codex app-server is closed")
	}
	c.pending[id] = waiter
	c.mu.Unlock()
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if err := c.send(message); err != nil {
		c.removePending(id)
		return err
	}
	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case response := <-waiter:
		if response.err != nil {
			return response.err
		}
		if output != nil && len(response.result) > 0 && string(response.result) != "null" {
			return json.Unmarshal(response.result, output)
		}
		return nil
	}
}

func (c *codexRPCClient) Notify(_ context.Context, method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return c.send(message)
}

func (c *codexRPCClient) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		select {
		case <-c.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.closed = true
	c.mu.Unlock()
	if c.command.Process != nil {
		_ = c.command.Process.Signal(os.Interrupt)
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(2 * time.Second):
		if c.command.Process != nil {
			_ = c.command.Process.Kill()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *codexRPCClient) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message codexRPCMessage
		if json.Unmarshal([]byte(line), &message) != nil {
			c.failAll(fmt.Errorf("Codex app-server emitted invalid JSON: %s", truncateRunes(line, 500)))
			return
		}
		if len(message.ID) > 0 && message.Method == "" {
			id, err := strconv.ParseInt(strings.Trim(string(message.ID), `"`), 10, 64)
			if err != nil {
				continue
			}
			c.mu.Lock()
			waiter := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if waiter == nil {
				continue
			}
			if message.Error != nil {
				waiter <- codexRPCResponse{err: fmt.Errorf("Codex %d: %s", message.Error.Code, message.Error.Message)}
			} else {
				waiter <- codexRPCResponse{result: message.Result}
			}
			continue
		}
		if message.Method == "" {
			continue
		}
		if len(message.ID) == 0 {
			if c.onNotification != nil {
				c.onNotification(context.Background(), message.Method, message.Params)
			}
			continue
		}
		go c.handleServerRequest(message)
	}
	if err := scanner.Err(); err != nil {
		c.failAll(err)
	}
}

func (c *codexRPCClient) handleServerRequest(message codexRPCMessage) {
	id := json.RawMessage(message.ID)
	if c.onRequest == nil {
		_ = c.send(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": "unsupported Codex request " + message.Method}})
		return
	}
	result, err := c.onRequest(context.Background(), message.Method, message.Params)
	if err != nil {
		_ = c.send(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": err.Error()}})
		return
	}
	_ = c.send(map[string]any{"id": id, "result": result})
}

func (c *codexRPCClient) readStderr(reader io.Reader) {
	payload, _ := io.ReadAll(io.LimitReader(reader, 1<<20))
	c.mu.Lock()
	_, _ = c.stderr.Write(payload)
	c.mu.Unlock()
}

func (c *codexRPCClient) send(message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return errors.New("Codex app-server stdin is closed")
	}
	_, err = c.stdin.Write(append(payload, '\n'))
	return err
}

func (c *codexRPCClient) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *codexRPCClient) failAll(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[int64]chan codexRPCResponse{}
	c.mu.Unlock()
	for _, waiter := range pending {
		waiter <- codexRPCResponse{err: err}
	}
}

func (c *codexRPCClient) stderrSuffix() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := strings.TrimSpace(c.stderr.String())
	if value == "" {
		return ""
	}
	return ": " + truncateRunes(value, 16384)
}
