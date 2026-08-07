package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
)

type ChatRunStatus string

const (
	ChatRunRunning   ChatRunStatus = "running"
	ChatRunSucceeded ChatRunStatus = "succeeded"
	ChatRunFailed    ChatRunStatus = "failed"
	ChatRunCanceled  ChatRunStatus = "canceled"
)

type ChatRun struct {
	ID          string        `json:"id"`
	ChatID      string        `json:"chat_id"`
	ProjectPath string        `json:"project_path"`
	ProjectID   string        `json:"project_id"`
	Status      ChatRunStatus `json:"status"`
	Question    string        `json:"question"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Error       string        `json:"error,omitempty"`
}

type ChatRunEvent struct {
	ID          string                `json:"id"`
	RunID       string                `json:"run_id"`
	Type        string                `json:"type"`
	Time        time.Time             `json:"time"`
	Step        int                   `json:"step,omitempty"`
	Action      *core.QueryAction     `json:"action,omitempty"`
	Message     string                `json:"message"`
	Observation string                `json:"observation,omitempty"`
	ElapsedMS   int64                 `json:"elapsed_ms"`
	Session     *ChatSession          `json:"session,omitempty"`
	Answer      *core.QueryAnswer     `json:"answer,omitempty"`
	Writeback   *QueryWritebackResult `json:"writeback,omitempty"`
}

type ChatRunFile struct {
	Version int            `json:"version"`
	Run     ChatRun        `json:"run"`
	Events  []ChatRunEvent `json:"events"`
}

type chatRunActive struct {
	cancel context.CancelFunc
}

var (
	chatRunMu          sync.Mutex
	activeChatRuns     = map[string]chatRunActive{}
	activeChatSessions = map[string]string{}
	chatRunSubscribers = map[string]map[chan ChatRunEvent]bool{}
)

func StartChatRun(opts ChatAppendOptions) (ChatRun, ChatSession, error) {
	if strings.TrimSpace(opts.ProjectPath) == "" {
		return ChatRun{}, ChatSession{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(opts.Question) == "" {
		return ChatRun{}, ChatSession{}, fmt.Errorf("question is required")
	}
	session, err := ReadChatSession(opts.ProjectPath, opts.SessionID)
	if os.IsNotExist(err) {
		session, err = CreateChatSession(opts.ProjectPath, "")
	}
	if err != nil {
		return ChatRun{}, ChatSession{}, err
	}
	sessionKey := chatRunSessionKey(opts.ProjectPath, session.ID)
	chatRunMu.Lock()
	if runID := activeChatSessions[sessionKey]; runID != "" {
		chatRunMu.Unlock()
		return ChatRun{}, ChatSession{}, fmt.Errorf("chat session already has a running query: %s", runID)
	}
	chatRunMu.Unlock()

	now := time.Now().UTC()
	session.Messages = append(session.Messages, ChatMessage{
		ID:        core.StableID("chat-message", session.ID, "user", now.Format(time.RFC3339Nano)),
		Role:      "user",
		Content:   opts.Question,
		CreatedAt: now,
	})
	if session.Title == "" || session.Title == "新的对话" {
		session.Title = titleFromQuestion(opts.Question)
	}
	session.UpdatedAt = now
	if err := saveChatSession(opts.ProjectPath, session); err != nil {
		return ChatRun{}, ChatSession{}, err
	}

	run := ChatRun{
		ID:          core.StableID("chat-run", session.ID, opts.Question, now.Format(time.RFC3339Nano)),
		ChatID:      session.ID,
		ProjectPath: opts.ProjectPath,
		ProjectID:   opts.ProjectID,
		Status:      ChatRunRunning,
		Question:    opts.Question,
		StartedAt:   now,
	}
	if err := saveChatRunFile(opts.ProjectPath, ChatRunFile{Version: 1, Run: run}); err != nil {
		return ChatRun{}, ChatSession{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := chatRunKey(opts.ProjectPath, run.ID)
	chatRunMu.Lock()
	activeChatRuns[key] = chatRunActive{cancel: cancel}
	activeChatSessions[sessionKey] = run.ID
	chatRunMu.Unlock()
	appendChatRunEvent(opts.ProjectPath, run, ChatRunEvent{
		Type:    "started",
		Message: "查询已启动",
	})
	runOpts := opts
	runOpts.SessionID = session.ID
	runOpts.Context = ctx
	go runChatRun(runOpts, run)
	return run, session, nil
}

func runChatRun(opts ChatAppendOptions, run ChatRun) {
	defer clearActiveChatRun(opts.ProjectPath, run.ChatID, run.ID)
	progress := func(event QueryProgressEvent) {
		appendChatRunEvent(opts.ProjectPath, run, ChatRunEvent{
			Type:        event.Type,
			Step:        event.Step,
			Action:      event.Action,
			Message:     event.Message,
			Observation: event.Observation,
		})
	}
	result, err := appendChatAssistantFromExistingUser(opts, progress)
	if err != nil {
		status := ChatRunFailed
		eventType := "error"
		message := err.Error()
		if opts.Context != nil && opts.Context.Err() != nil {
			status = ChatRunCanceled
			eventType = "canceled"
			message = "查询已取消"
		}
		_ = finishChatRun(opts.ProjectPath, run.ID, status, message)
		appendChatRunEvent(opts.ProjectPath, run, ChatRunEvent{Type: eventType, Message: message})
		return
	}
	if result.Writeback != nil {
		appendChatRunEvent(opts.ProjectPath, run, ChatRunEvent{
			Type:      "writeback_done",
			Message:   "综合页已写回",
			Writeback: result.Writeback,
		})
	}
	_ = finishChatRun(opts.ProjectPath, run.ID, ChatRunSucceeded, "")
	appendChatRunEvent(opts.ProjectPath, run, ChatRunEvent{
		Type:      "completed",
		Message:   "查询完成",
		Session:   &result.Session,
		Answer:    result.Answer,
		Writeback: result.Writeback,
	})
}

func appendChatAssistantFromExistingUser(opts ChatAppendOptions, progress QueryProgressFunc) (ChatAppendResult, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := ReadChatSession(opts.ProjectPath, opts.SessionID)
	if err != nil {
		return ChatAppendResult{}, err
	}
	conversation := chatConversationContext(session.Messages)
	answer, err := QueryLLMWikiWithOptions(QueryOptions{
		ProjectPath:         opts.ProjectPath,
		ProjectID:           opts.ProjectID,
		Question:            opts.Question,
		ConversationContext: conversation,
		Limit:               opts.Limit,
		Agent:               opts.Agent,
		SearchStore:         opts.SearchStore,
		GraphStore:          opts.GraphStore,
		QueryLogStore:       opts.QueryLogStore,
		EmbeddingProvider:   opts.EmbeddingProvider,
		Context:             ctx,
		Progress:            progress,
		Runtime:             opts.Runtime,
	})
	if err != nil {
		return ChatAppendResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ChatAppendResult{}, err
	}
	var writeback *QueryWritebackResult
	if strings.TrimSpace(opts.SaveTitle) != "" {
		if !answer.Plan.CanWriteBack {
			return ChatAppendResult{}, fmt.Errorf("query answer is not eligible for writeback; configure an LLM query agent")
		}
		title := opts.SaveTitle
		if title == "auto" {
			title = answer.SuggestedWritebackTitle
		}
		wb, err := WriteQueryAnswer(QueryWritebackOptions{
			ProjectPath: opts.ProjectPath,
			Title:       title,
			Answer:      answer,
		})
		if err != nil {
			return ChatAppendResult{}, err
		}
		writeback = &wb
		if opts.OnWriteback != nil {
			if err := opts.OnWriteback(ctx, wb); err != nil {
				return ChatAppendResult{}, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ChatAppendResult{}, err
	}
	assistantTime := time.Now().UTC()
	session.Messages = append(session.Messages, ChatMessage{
		ID:        core.StableID("chat-message", session.ID, "assistant", assistantTime.Format(time.RFC3339Nano)),
		Role:      "assistant",
		Content:   answer.Answer,
		Answer:    answer,
		Writeback: writeback,
		CreatedAt: assistantTime,
	})
	session.UpdatedAt = assistantTime
	if err := saveChatSession(opts.ProjectPath, session); err != nil {
		return ChatAppendResult{}, err
	}
	return ChatAppendResult{Session: session, Answer: answer, Writeback: writeback}, nil
}

func CancelChatRun(projectPath, runID string) (ChatRun, error) {
	key := chatRunKey(projectPath, runID)
	chatRunMu.Lock()
	active := activeChatRuns[key]
	chatRunMu.Unlock()
	if active.cancel != nil {
		active.cancel()
	}
	_ = finishChatRun(projectPath, runID, ChatRunCanceled, "查询已取消")
	file, err := loadChatRunFile(projectPath, runID)
	if err != nil {
		return ChatRun{}, err
	}
	appendChatRunEvent(projectPath, file.Run, ChatRunEvent{Type: "canceled", Message: "查询已取消"})
	return file.Run, nil
}

func SubscribeChatRun(projectPath, runID string) (ChatRunFile, <-chan ChatRunEvent, func(), error) {
	file, err := loadChatRunFile(projectPath, runID)
	if err != nil {
		return ChatRunFile{}, nil, nil, err
	}
	ch := make(chan ChatRunEvent, 32)
	key := chatRunKey(projectPath, runID)
	chatRunMu.Lock()
	if chatRunSubscribers[key] == nil {
		chatRunSubscribers[key] = map[chan ChatRunEvent]bool{}
	}
	chatRunSubscribers[key][ch] = true
	chatRunMu.Unlock()
	unsubscribe := func() {
		chatRunMu.Lock()
		delete(chatRunSubscribers[key], ch)
		if len(chatRunSubscribers[key]) == 0 {
			delete(chatRunSubscribers, key)
		}
		chatRunMu.Unlock()
		close(ch)
	}
	return file, ch, unsubscribe, nil
}

func LoadChatRun(projectPath, runID string) (ChatRunFile, error) {
	return loadChatRunFile(projectPath, runID)
}

func appendChatRunEvent(projectPath string, run ChatRun, event ChatRunEvent) {
	chatRunMu.Lock()
	defer chatRunMu.Unlock()
	file, err := loadChatRunFileLocked(projectPath, run.ID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	event.RunID = run.ID
	event.Time = now
	event.ElapsedMS = now.Sub(file.Run.StartedAt).Milliseconds()
	event.ID = fmt.Sprintf("%s-%04d", run.ID, len(file.Events)+1)
	file.Events = append(file.Events, event)
	_ = saveChatRunFileLocked(projectPath, file)
	for ch := range chatRunSubscribers[chatRunKey(projectPath, run.ID)] {
		select {
		case ch <- event:
		default:
		}
	}
}

func finishChatRun(projectPath, runID string, status ChatRunStatus, message string) error {
	chatRunMu.Lock()
	defer chatRunMu.Unlock()
	file, err := loadChatRunFileLocked(projectPath, runID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	file.Run.Status = status
	file.Run.FinishedAt = &now
	file.Run.Error = message
	return saveChatRunFileLocked(projectPath, file)
}

func clearActiveChatRun(projectPath, chatID, runID string) {
	chatRunMu.Lock()
	defer chatRunMu.Unlock()
	delete(activeChatRuns, chatRunKey(projectPath, runID))
	delete(activeChatSessions, chatRunSessionKey(projectPath, chatID))
}

func loadChatRunFile(projectPath, runID string) (ChatRunFile, error) {
	chatRunMu.Lock()
	defer chatRunMu.Unlock()
	return loadChatRunFileLocked(projectPath, runID)
}

func loadChatRunFileLocked(projectPath, runID string) (ChatRunFile, error) {
	if strings.TrimSpace(runID) == "" || strings.Contains(runID, "/") || strings.Contains(runID, "\\") {
		return ChatRunFile{}, fmt.Errorf("invalid chat run id")
	}
	data, err := os.ReadFile(chatRunPath(projectPath, runID))
	if err != nil {
		return ChatRunFile{}, err
	}
	var file ChatRunFile
	if err := json.Unmarshal(data, &file); err != nil {
		return ChatRunFile{}, fmt.Errorf("read chat run: %w", err)
	}
	return file, nil
}

func saveChatRunFile(projectPath string, file ChatRunFile) error {
	chatRunMu.Lock()
	defer chatRunMu.Unlock()
	return saveChatRunFileLocked(projectPath, file)
}

func saveChatRunFileLocked(projectPath string, file ChatRunFile) error {
	file.Version = 1
	if err := os.MkdirAll(chatRunDir(projectPath), 0o755); err != nil {
		return err
	}
	sort.SliceStable(file.Events, func(i, j int) bool { return file.Events[i].ID < file.Events[j].ID })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := chatRunPath(projectPath, file.Run.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, chatRunPath(projectPath, file.Run.ID))
}

func chatRunDir(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "chat-runs")
}

func chatRunPath(projectPath, runID string) string {
	return filepath.Join(chatRunDir(projectPath), runID+".json")
}

func chatRunKey(projectPath, runID string) string {
	return filepath.Clean(projectPath) + "\x00" + runID
}

func chatRunSessionKey(projectPath, chatID string) string {
	return filepath.Clean(projectPath) + "\x00" + chatID
}
