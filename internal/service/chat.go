package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type ChatSession struct {
	ID          string        `json:"id"`
	ProjectPath string        `json:"project_path"`
	Title       string        `json:"title"`
	Messages    []ChatMessage `json:"messages"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type ChatMessage struct {
	ID        string                `json:"id"`
	Role      string                `json:"role"`
	Content   string                `json:"content"`
	Answer    *core.QueryAnswer     `json:"answer,omitempty"`
	Writeback *QueryWritebackResult `json:"writeback,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
}

type ChatAppendOptions struct {
	ProjectPath       string
	ProjectID         string
	SessionID         string
	Question          string
	Agent             QueryAgent
	Limit             int
	SaveTitle         string
	SearchStore       SearchEvidenceStore
	GraphStore        GraphEvidenceStore
	QueryLogStore     QueryLogStore
	EmbeddingProvider EmbeddingProvider
	Context           context.Context
	Progress          QueryProgressFunc
	OnWriteback       func(context.Context, QueryWritebackResult) error
}

type ChatAppendResult struct {
	Session   ChatSession           `json:"session"`
	Answer    *core.QueryAnswer     `json:"answer"`
	Writeback *QueryWritebackResult `json:"writeback,omitempty"`
}

func ListChatSessions(projectPath string) ([]ChatSession, error) {
	dir := chatDir(projectPath)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []ChatSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		session, err := ReadChatSession(projectPath, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	return sessions, nil
}

func CreateChatSession(projectPath, title string) (ChatSession, error) {
	now := time.Now().UTC()
	session := ChatSession{
		ID:          core.StableID("chat", projectPath, now.Format(time.RFC3339Nano)),
		ProjectPath: projectPath,
		Title:       firstNonEmptyString(strings.TrimSpace(title), "新的对话"),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := saveChatSession(projectPath, session); err != nil {
		return ChatSession{}, err
	}
	return session, nil
}

func ReadChatSession(projectPath, id string) (ChatSession, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		return ChatSession{}, fmt.Errorf("invalid chat id")
	}
	data, err := os.ReadFile(filepath.Join(chatDir(projectPath), id+".json"))
	if err != nil {
		return ChatSession{}, err
	}
	var session ChatSession
	if err := json.Unmarshal(data, &session); err != nil {
		return ChatSession{}, fmt.Errorf("read chat session: %w", err)
	}
	return session, nil
}

func DeleteChatSession(projectPath, id string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		return fmt.Errorf("invalid chat id")
	}
	err := os.Remove(filepath.Join(chatDir(projectPath), id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func AppendChatMessage(opts ChatAppendOptions) (ChatAppendResult, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := ReadChatSession(opts.ProjectPath, opts.SessionID)
	if os.IsNotExist(err) {
		session, err = CreateChatSession(opts.ProjectPath, "")
	}
	if err != nil {
		return ChatAppendResult{}, err
	}
	if strings.TrimSpace(opts.Question) == "" {
		return ChatAppendResult{}, fmt.Errorf("question is required")
	}
	now := time.Now().UTC()
	userMessage := ChatMessage{
		ID:        core.StableID("chat-message", session.ID, "user", now.Format(time.RFC3339Nano)),
		Role:      "user",
		Content:   opts.Question,
		CreatedAt: now,
	}
	session.Messages = append(session.Messages, userMessage)
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
		Progress:            opts.Progress,
	})
	if err != nil {
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
	if session.Title == "" || session.Title == "新的对话" {
		session.Title = titleFromQuestion(opts.Question)
	}
	session.UpdatedAt = assistantTime
	if err := saveChatSession(opts.ProjectPath, session); err != nil {
		return ChatAppendResult{}, err
	}
	return ChatAppendResult{Session: session, Answer: answer, Writeback: writeback}, nil
}

func saveChatSession(projectPath string, session ChatSession) error {
	if strings.TrimSpace(projectPath) == "" {
		return fmt.Errorf("project path is required")
	}
	if session.ID == "" {
		return fmt.Errorf("chat id is required")
	}
	dir := chatDir(projectPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, session.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, session.ID+".json"))
}

func chatDir(projectPath string) string {
	return filepath.Join(projectPath, ".kbcore", "chats")
}

func chatConversationContext(messages []ChatMessage) string {
	if len(messages) == 0 {
		return ""
	}
	start := 0
	if len(messages) > 8 {
		start = len(messages) - 8
	}
	var b strings.Builder
	for _, message := range messages[start:] {
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", message.Role, promptSnippet(message.Content, 800))
	}
	return strings.TrimSpace(b.String())
}

func titleFromQuestion(question string) string {
	title := strings.TrimSpace(question)
	runes := []rune(title)
	if len(runes) > 32 {
		title = string(runes[:32])
	}
	if title == "" {
		title = "新的对话"
	}
	return title
}
