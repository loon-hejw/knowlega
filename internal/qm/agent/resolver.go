package agent

import (
	"context"

	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type RuntimeConfigStore interface {
	Selection(context.Context, string) (*data.RuntimeSelection, error)
	LegacyModel(context.Context, string) (string, error)
	ApprovedHarnesses(context.Context, string) ([]string, error)
}

type DurableChoiceResolver struct {
	Models config.ModelsConfig
	Store  RuntimeConfigStore
}

func (r DurableChoiceResolver) Resolve(ctx context.Context, input TurnInput, fallback Choice) (Choice, error) {
	approved, err := r.Store.ApprovedHarnesses(ctx, input.OrgScopeID)
	if err != nil {
		return Choice{}, err
	}
	if len(approved) == 0 {
		for _, harness := range r.Models.Harnesses {
			approved = append(approved, harness.ID)
		}
	}
	approvedSet := map[string]bool{}
	for _, id := range approved {
		if _, configured := r.Models.Harness(id); configured {
			approvedSet[id] = true
		}
	}
	safeFallback := fallback
	if !r.valid(safeFallback, approvedSet) {
		for _, harness := range r.Models.Harnesses {
			if approvedSet[harness.ID] {
				safeFallback = Choice{HarnessID: harness.ID, ModelID: harness.DefaultModel}
				break
			}
		}
	}
	org := safeFallback
	orgSelection, err := r.Store.Selection(ctx, input.OrgScopeID)
	if err != nil {
		return Choice{}, err
	}
	orgLegacy, err := r.Store.LegacyModel(ctx, input.OrgScopeID)
	if err != nil {
		return Choice{}, err
	}
	if orgSelection != nil {
		candidate := Choice{HarnessID: orgSelection.HarnessID, ModelID: orgSelection.ModelID}
		if r.valid(candidate, approvedSet) {
			org = candidate
		}
	} else if orgLegacy != "" {
		candidate := Choice{HarnessID: safeFallback.HarnessID, ModelID: orgLegacy}
		if r.valid(candidate, approvedSet) {
			org = candidate
		}
	}
	if input.ScopeLabel == "" || input.ScopeLabel == input.OrgScopeID {
		return org, nil
	}
	scopedSelection, err := r.Store.Selection(ctx, input.ScopeLabel)
	if err != nil {
		return Choice{}, err
	}
	scopedLegacy, err := r.Store.LegacyModel(ctx, input.ScopeLabel)
	if err != nil {
		return Choice{}, err
	}
	candidate := org
	if scopedSelection != nil {
		candidate = Choice{HarnessID: scopedSelection.HarnessID, ModelID: scopedSelection.ModelID}
	} else if scopedLegacy != "" {
		candidate = Choice{HarnessID: safeFallback.HarnessID, ModelID: scopedLegacy}
	}
	if !r.valid(candidate, approvedSet) {
		return org, nil
	}
	return candidate, nil
}

func (r DurableChoiceResolver) valid(choice Choice, approved map[string]bool) bool {
	if !approved[choice.HarnessID] {
		return false
	}
	harness, ok := r.Models.Harness(choice.HarnessID)
	return ok && contains(harness.ModelIDs, choice.ModelID)
}
