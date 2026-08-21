package agent

import (
	"context"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type memoryRuntimeConfig struct {
	selections map[string]*data.RuntimeSelection
	legacy     map[string]string
	approved   []string
}

func (s memoryRuntimeConfig) Selection(_ context.Context, scope string) (*data.RuntimeSelection, error) {
	return s.selections[scope], nil
}
func (s memoryRuntimeConfig) LegacyModel(_ context.Context, scope string) (string, error) {
	return s.legacy[scope], nil
}
func (s memoryRuntimeConfig) ApprovedHarnesses(context.Context, string) ([]string, error) {
	return s.approved, nil
}

func TestDurableChoiceResolverMatchesNodeInheritance(t *testing.T) {
	models := testModels()
	resolver := DurableChoiceResolver{Models: models, Store: memoryRuntimeConfig{
		selections: map[string]*data.RuntimeSelection{
			"org:acme":       {HarnessID: "codex", ModelID: "model-codex"},
			"personal:alice": {HarnessID: "pi", ModelID: "model-pi"},
		},
		legacy:   map[string]string{},
		approved: []string{"pi", "codex"},
	}}
	choice, err := resolver.Resolve(context.Background(), TurnInput{OrgScopeID: "org:acme", ScopeLabel: "personal:alice"}, Choice{HarnessID: "pi", ModelID: "model-pi"})
	if err != nil || choice != (Choice{HarnessID: "pi", ModelID: "model-pi"}) {
		t.Fatalf("choice=%#v err=%v", choice, err)
	}
	choice, err = resolver.Resolve(context.Background(), TurnInput{OrgScopeID: "org:acme", ScopeLabel: "personal:bob"}, Choice{HarnessID: "pi", ModelID: "model-pi"})
	if err != nil || choice != (Choice{HarnessID: "codex", ModelID: "model-codex"}) {
		t.Fatalf("inherited choice=%#v err=%v", choice, err)
	}
}

func TestDurableChoiceResolverFallsBackWhenPersistedChoiceIsNotApproved(t *testing.T) {
	models := testModels()
	resolver := DurableChoiceResolver{Models: models, Store: memoryRuntimeConfig{
		selections: map[string]*data.RuntimeSelection{"org:acme": {HarnessID: "pi", ModelID: "model-pi"}},
		legacy:     map[string]string{},
		approved:   []string{"codex"},
	}}
	choice, err := resolver.Resolve(context.Background(), TurnInput{OrgScopeID: "org:acme", ScopeLabel: "personal:alice"}, Choice{HarnessID: "pi", ModelID: "model-pi"})
	if err != nil || choice != (Choice{HarnessID: "codex", ModelID: "model-codex"}) {
		t.Fatalf("choice=%#v err=%v", choice, err)
	}
}
