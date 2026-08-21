package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type memorySelectionStore struct{ selected map[string]string }

func (s *memorySelectionStore) Select(_ context.Context, sessionID, harnessID string) (string, bool, error) {
	previous := s.selected[sessionID]
	s.selected[sessionID] = harnessID
	return previous, previous != "" && previous != harnessID, nil
}

func (s *memorySelectionStore) Delete(_ context.Context, sessionID string) error {
	delete(s.selected, sessionID)
	return nil
}

type fakeAdapter struct {
	profile Profile
	runs    []TurnInput
	resets  []string
	closed  int
}

func (a *fakeAdapter) Profile() Profile { return a.profile }
func (a *fakeAdapter) RunTurn(_ context.Context, input TurnInput) (TurnResult, error) {
	a.runs = append(a.runs, input)
	return TurnResult{Reply: input.Harness + "/" + input.Model}, nil
}
func (a *fakeAdapter) ResetSession(_ context.Context, sessionID string) error {
	a.resets = append(a.resets, sessionID)
	return nil
}
func (a *fakeAdapter) Close(context.Context) error { a.closed++; return nil }

func TestProfilesMatchNodeAdapters(t *testing.T) {
	profiles := Profiles()
	checks := map[string]struct {
		control, tools, transcript string
		capabilities               []Capability
	}{
		"pi":       {"in-process", "in-process", "pi", []Capability{CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityThinkingLevel, CapabilityFastMode, CapabilityProviderSessions}},
		"opencode": {"http", "plugin", "opencode", []Capability{CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityProviderSessions}},
		"codex":    {"json-rpc", "dynamic", "responses-api", []Capability{CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityProviderSessions}},
		"claude":   {"sdk", "in-process-mcp", "claude-agent-sdk", []Capability{CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityThinkingLevel, CapabilityFastMode}},
		"mock":     {"mock", "mock", "qm", nil},
	}
	for id, check := range checks {
		got := profiles[id]
		if string(got.ControlTransport) != check.control || string(got.ToolTransport) != check.tools || got.TranscriptFormat != check.transcript {
			t.Fatalf("profile %s=%#v", id, got)
		}
		for _, capability := range check.capabilities {
			if !got.Capabilities[capability] {
				t.Fatalf("profile %s lacks %s", id, capability)
			}
		}
	}
}

func TestEngineRoutesConfiguredHarnessAndResetsOnSwitch(t *testing.T) {
	models := testModels()
	pi := &fakeAdapter{profile: Profiles()["pi"]}
	codex := &fakeAdapter{profile: Profiles()["codex"]}
	store := &memorySelectionStore{selected: map[string]string{}}
	engine, err := NewEngine(models, store, []Adapter{pi, codex}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.RunTurn(context.Background(), TurnInput{SessionID: "s1", Input: "hello"})
	if err != nil || first.Reply != "pi/model-pi" || len(pi.resets) != 0 {
		t.Fatalf("first=%#v pi=%#v err=%v", first, pi, err)
	}
	second, err := engine.RunTurn(context.Background(), TurnInput{SessionID: "s1", Harness: "codex", Model: "model-codex"})
	if err != nil || second.Reply != "codex/model-codex" || !reflect.DeepEqual(pi.resets, []string{"s1"}) || !reflect.DeepEqual(codex.resets, []string{"s1"}) {
		t.Fatalf("second=%#v pi=%#v codex=%#v err=%v", second, pi, codex, err)
	}
}

func TestEngineRejectsUnapprovedAndUnavailableHarness(t *testing.T) {
	models := testModels()
	engine, err := NewEngine(models, &memorySelectionStore{selected: map[string]string{}}, []Adapter{&fakeAdapter{profile: Profiles()["pi"]}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.RunTurn(context.Background(), TurnInput{SessionID: "s1", Harness: "pi", Model: "model-codex"})
	if err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("err=%v", err)
	}
	_, err = engine.RunTurn(context.Background(), TurnInput{SessionID: "s1", Harness: "codex", Model: "model-codex"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err=%v", err)
	}
}

func TestEngineRejectsAdapterProfileDrift(t *testing.T) {
	bad := Profiles()["codex"]
	bad.ToolTransport = "in-process"
	_, err := NewEngine(testModels(), &memorySelectionStore{selected: map[string]string{}}, []Adapter{&fakeAdapter{profile: bad}}, nil)
	if err == nil || !strings.Contains(err.Error(), "Node contract") {
		t.Fatalf("err=%v", err)
	}
}

func testModels() config.ModelsConfig {
	return config.ModelsConfig{
		DefaultHarness: "pi",
		Providers: []config.ModelProviderConfig{
			{ID: "pi-provider", Protocol: "openai", Models: []config.ModelDefinition{{ID: "model-pi"}}},
			{ID: "codex-provider", Protocol: "openai", Models: []config.ModelDefinition{{ID: "model-codex"}}},
		},
		Harnesses: []config.ModelHarnessConfig{
			{ID: "pi", Provider: "pi-provider", ModelIDs: []string{"model-pi"}, DefaultModel: "model-pi"},
			{ID: "codex", Provider: "codex-provider", ModelIDs: []string{"model-codex"}, DefaultModel: "model-codex"},
		},
	}
}
