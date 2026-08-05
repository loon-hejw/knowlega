package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
	"github.com/hejw/knowledge-core/internal/wiki"
)

type chatRunTestAgent struct{}

func (chatRunTestAgent) PlanQuery(input QueryPlanningInput) (core.QueryPlan, error) {
	return core.QueryPlan{
		Question:       input.Question,
		Intent:         "answer_from_persistent_wiki",
		ReadFirst:      []string{"wiki/index.md"},
		CandidateLimit: 3,
		AnswerMode:     "llm_synthesis",
		CanWriteBack:   false,
	}, nil
}

func (chatRunTestAgent) SynthesizeQuery(QuerySynthesisInput) (string, error) {
	return "answer from test agent", nil
}

func TestStartChatRunStreamsProgressAndCompletesSession(t *testing.T) {
	root := t.TempDir()
	session, err := CreateChatSession(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	run, startedSession, err := StartChatRun(ChatAppendOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		SessionID:   session.ID,
		Question:    "What happened?",
		Agent:       chatRunTestAgent{},
		Limit:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != ChatRunRunning {
		t.Fatalf("run status=%s", run.Status)
	}
	if len(startedSession.Messages) != 1 || startedSession.Messages[0].Role != "user" {
		t.Fatalf("started session messages=%+v", startedSession.Messages)
	}
	deadline := time.After(2 * time.Second)
	for {
		file, err := LoadChatRun(root, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if file.Run.Status == ChatRunSucceeded {
			if len(file.Events) == 0 {
				t.Fatalf("expected progress events")
			}
			updated, err := ReadChatSession(root, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.Messages) != 2 || updated.Messages[1].Role != "assistant" {
				t.Fatalf("updated session messages=%+v", updated.Messages)
			}
			if updated.Messages[1].Content != "answer from test agent" {
				t.Fatalf("assistant content=%q", updated.Messages[1].Content)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("run did not complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestStartChatRunDegradesWhenActionParsingFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kb")
	if err := wiki.InitProject(wiki.ProjectOptions{Path: root, Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "oauth.md"), `---
type: "concept"
title: "OAuth Token Validation"
---

# OAuth Token Validation

Token validation calls AuthService.
`)
	session, err := CreateChatSession(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	agent := &failingActionAgent{
		plan: core.QueryPlan{
			Question:       "How is token validation handled?",
			Intent:         "answer_from_persistent_wiki",
			ReadFirst:      []string{"wiki/concepts/oauth.md"},
			CandidateLimit: 3,
			AnswerMode:     "llm_tool_loop",
			CanWriteBack:   false,
		},
		actionErr: errors.New(`parse llm query action: unexpected end of JSON input: {"action":"writeback","title":"`),
	}
	run, _, err := StartChatRun(ChatAppendOptions{
		ProjectPath: root,
		ProjectID:   "project-1",
		SessionID:   session.ID,
		Question:    "How is token validation handled?",
		Agent:       agent,
		Limit:       3,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		file, err := LoadChatRun(root, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if file.Run.Status == ChatRunSucceeded {
			if !chatRunEventsContain(file.Events, "action_failed") {
				t.Fatalf("expected action_failed event, got %+v", file.Events)
			}
			if chatRunEventsContain(file.Events, "synthesis_started") {
				t.Fatalf("action failure must not trigger synthesis: %+v", file.Events)
			}
			updated, err := ReadChatSession(root, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.Messages) != 2 || updated.Messages[1].Content == "" {
				t.Fatalf("updated session messages=%+v", updated.Messages)
			}
			return
		}
		if file.Run.Status == ChatRunFailed {
			t.Fatalf("run failed: %+v", file.Run)
		}
		select {
		case <-deadline:
			t.Fatalf("run did not complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func chatRunEventsContain(events []ChatRunEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}
