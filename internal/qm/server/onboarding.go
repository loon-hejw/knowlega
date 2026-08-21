package server

import (
	"net/http"
	"strings"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func (h *HTTPServer) yamlOnboardingRoute(method, path string) bool {
	switch {
	case method == http.MethodGet && (path == "/v1/admin/onboarding" || path == "/v1/admin/model-providers" || path == "/v1/admin/custom-providers" || path == "/v1/admin/slack-installation"):
		return true
	case method == http.MethodGet && (path == "/v1/connectors/catalog" || path == "/v1/connectors/oauth/status"):
		return true
	case (method == http.MethodPut || method == http.MethodDelete) && yamlManagedItemPath(path, "/v1/admin/model-providers/"):
		return true
	case (method == http.MethodPut || method == http.MethodDelete) && yamlManagedItemPath(path, "/v1/admin/custom-providers/"):
		return true
	case (method == http.MethodPut || method == http.MethodDelete) && path == "/v1/admin/slack-installation":
		return true
	case method == http.MethodPut:
		parts := strings.Split(strings.Trim(path, "/"), "/")
		return len(parts) == 5 && parts[0] == "v1" && parts[1] == "admin" && parts[2] == "scopes" && parts[3] != "" && (parts[4] == "base-model" || parts[4] == "connectors")
	default:
		return false
	}
}

func yamlManagedItemPath(path, prefix string) bool {
	item := strings.TrimPrefix(path, prefix)
	return item != path && item != "" && !strings.Contains(item, "/")
}

func safeModelProviders(models config.ModelsConfig) []map[string]any {
	providers := make([]map[string]any, 0, len(models.Providers))
	for _, provider := range models.Providers {
		listed := make([]map[string]any, 0, len(provider.Models))
		for _, model := range provider.Models {
			listed = append(listed, map[string]any{
				"id": model.ID, "name": model.Name, "provider": provider.ID,
				"contextWindow": model.ContextWindow, "maxTokens": model.MaxTokens,
			})
		}
		providers = append(providers, map[string]any{
			"id": provider.ID, "protocol": provider.Protocol, "baseUrl": provider.BaseURL,
			"configured": provider.Protocol == "mock" || strings.TrimSpace(provider.APIKey) != "",
			"source":     "yaml", "models": listed,
		})
	}
	return providers
}

func (h *HTTPServer) adminOnboarding(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	scope := "org:" + h.config.QM.OrgID
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "onboarding.read", Resource: "onboarding", ScopeLabel: scope, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}

	baseModel := h.config.QM.Models.DefaultModel()
	provider, modelConfigured := h.config.QM.Models.ProviderForModel(baseModel)
	modelName := baseModel
	for _, candidate := range provider.Models {
		if candidate.ID == baseModel {
			modelName = candidate.Name
			break
		}
	}
	customProviders := 0
	for _, candidate := range h.config.QM.Models.Providers {
		if candidate.Protocol != "mock" {
			customProviders++
		}
	}
	oauthProviders := make([]string, 0, len(h.config.QM.OAuth.Clients))
	for _, client := range h.config.QM.OAuth.Clients {
		oauthProviders = append(oauthProviders, client.Provider)
	}
	slackConfigured := strings.TrimSpace(h.config.QM.Slack.BotToken) != "" && strings.TrimSpace(h.config.QM.Slack.AppToken) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"model": map[string]any{
			"configured": modelConfigured, "id": baseModel, "name": modelName,
			"defaultHarness": h.config.QM.Models.DefaultHarness,
			"provider":       provider.ID, "providerCount": len(h.config.QM.Models.Providers), "customProviderCount": customProviders,
			"source": "yaml", "configPath": "qm.models",
		},
		"slack": map[string]any{
			"configured": slackConfigured, "teamId": h.config.QM.Slack.TeamID, "teamName": h.config.QM.Slack.TeamName,
			"source": "yaml", "configPath": "qm.slack",
		},
		"oauth": map[string]any{
			"configured": len(oauthProviders) > 0, "configuredProviders": oauthProviders,
			"source": "yaml", "configPath": "qm.oauth.clients",
		},
	})
}

func (h *HTTPServer) adminModelProviders(w http.ResponseWriter, r *http.Request, identity auth.Identity) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	providers := safeModelProviders(h.config.QM.Models)
	models := make([]any, 0)
	statuses := make([]map[string]any, 0, len(providers))
	for _, provider := range providers {
		statuses = append(statuses, map[string]any{
			"provider": provider["id"], "configured": provider["configured"], "source": "yaml",
		})
		for _, model := range provider["models"].([]map[string]any) {
			models = append(models, model)
		}
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "model-providers.read", Resource: "model-providers", ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": statuses, "models": models, "baseModel": h.config.QM.Models.DefaultModel(), "defaultHarness": h.config.QM.Models.DefaultHarness, "harnesses": h.config.QM.Models.Harnesses, "source": "yaml"})
}

func (h *HTTPServer) yamlManaged(w http.ResponseWriter, r *http.Request, identity auth.Identity, configPath string) {
	actor, ok := h.requireOrgAdmin(w, r, identity)
	if !ok {
		return
	}
	_ = h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: actor, Action: "yaml-managed.write-rejected", Resource: r.URL.Path, ScopeLabel: "org:" + h.config.QM.OrgID, IdempotencyKey: requestID(r)})
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "yaml_managed", "message": "This configuration is managed by QM YAML.", "config_path": configPath,
	})
}
