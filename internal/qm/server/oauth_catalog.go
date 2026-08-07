package server

import (
	"net/http"
	"strings"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

// oauthCatalogProvider is the public, non-secret subset of Node's OAuth
// provider definitions. Keep this catalog aligned with qm/src/connectors/oauth.ts:
// OAuth callbacks, client resolution, refresh, and token exchange remain Node
// runtime work, while this describes the safe control-plane read projection.
type oauthCatalogProvider struct {
	Name         string
	Hosts        []string
	Scopes       []string
	ConsentMode  string
	RedirectPath string
	SetupGuide   map[string]any
}

var oauthCatalogProviders = []oauthCatalogProvider{
	{
		Name:        "google",
		Hosts:       []string{"gmail.googleapis.com", "www.googleapis.com", "sheets.googleapis.com", "docs.googleapis.com", "slides.googleapis.com"},
		Scopes:      []string{"https://www.googleapis.com/auth/gmail.modify", "https://www.googleapis.com/auth/calendar", "https://www.googleapis.com/auth/drive", "https://www.googleapis.com/auth/spreadsheets", "https://www.googleapis.com/auth/tasks", "openid", "email"},
		ConsentMode: "standard", RedirectPath: "google/callback",
		SetupGuide: map[string]any{"console": "Google Cloud Console → APIs & Services → Credentials", "url": "https://console.cloud.google.com/auth/clients", "steps": []string{"Create an OAuth 2.0 Client ID (type: Web application).", "Add the redirect URI shown below to 'Authorized redirect URIs' on YOUR client.", "Enable the Gmail, Calendar, Drive, Sheets, Docs, Slides, and Tasks APIs for the project.", "Choose admin-consent (domain-wide) vs per-user consent on the OAuth consent screen.", "Paste the Client ID + Client secret below; we validate by dry-running the consent URL."}, "scopesRationale": "gmail.modify/calendar/drive/spreadsheets/tasks back the Google Workspace skills; openid+email identify the account."},
	},
	{
		Name: "slack", Hosts: []string{"slack.com"},
		Scopes:      []string{"users:read", "channels:read", "channels:history", "groups:read", "groups:history", "im:read", "im:history", "mpim:read", "mpim:history", "chat:write", "canvases:read", "canvases:write", "search:read", "search:read.im", "search:read.mpim"},
		ConsentMode: "standard", RedirectPath: "slack/callback",
		SetupGuide: map[string]any{"console": "Slack API → Your Apps → OAuth & Permissions", "url": "https://api.slack.com/apps", "steps": []string{"Create a Slack app (from scratch) in your workspace.", "Under OAuth & Permissions, add the redirect URL shown below.", "Add the requested USER token scopes under 'User Token Scopes'.", "Install the app to the workspace.", "Paste the Client ID + Client secret below."}, "scopesRationale": "User-token scopes let the agent read/post, search, and edit canvases as the connecting user, not a bot."},
	},
	{
		Name: "notion", Hosts: []string{"api.notion.com"}, Scopes: []string{}, ConsentMode: "standard", RedirectPath: "notion/callback",
		SetupGuide: map[string]any{"console": "Notion → My integrations → New integration (Public)", "url": "https://www.notion.so/profile/integrations", "steps": []string{"Create a public OAuth integration.", "Add the redirect URI shown below to the integration's redirect URIs.", "Configure the capabilities the integration needs.", "Paste the Client ID (OAuth client ID) + the OAuth client secret below."}, "scopesRationale": "Notion grants access per selected pages at consent time (no OAuth scope list)."},
	},
	{
		Name: "linear", Hosts: []string{"api.linear.app"}, Scopes: []string{"read", "write"}, ConsentMode: "standard", RedirectPath: "linear/callback",
		SetupGuide: map[string]any{"console": "Linear → Settings → API → OAuth applications", "url": "https://linear.app/settings/api/applications", "steps": []string{"Create an OAuth application.", "Add the redirect URI shown below as a callback URL.", "Select the read/write scopes.", "Paste the Client ID + Client secret below."}, "scopesRationale": "read/write let the agent query and update issues, projects, and comments."},
	},
	{
		Name: "dropbox", Hosts: []string{"api.dropboxapi.com", "content.dropboxapi.com"},
		Scopes: []string{"account_info.read", "files.metadata.read", "files.content.read", "files.content.write", "sharing.read", "sharing.write"}, ConsentMode: "standard", RedirectPath: "dropbox/callback",
		SetupGuide: map[string]any{"console": "Dropbox App Console → Create app", "url": "https://www.dropbox.com/developers/apps/create", "steps": []string{"Create an app: Scoped access, Full Dropbox access.", "On the app's OAuth 2 section, add the redirect URI shown below.", "On the Permissions tab, enable account_info.read, files.metadata.read, files.content.read, files.content.write, sharing.read, sharing.write — then Submit.", "Paste the App key (Client ID) + App secret (Client secret) below."}, "scopesRationale": "files.content/metadata read+write back browse/download/upload; sharing.read/write let the agent manage shared links; account_info.read identifies the account."},
	},
	{
		Name: "github", Hosts: []string{"api.github.com"}, Scopes: []string{"repo", "read:org"}, ConsentMode: "github_app", RedirectPath: "github/callback",
		SetupGuide: map[string]any{"console": "GitHub → Settings → Developer settings → OAuth Apps (or GitHub App)", "url": "https://github.com/settings/applications/new", "steps": []string{"Register a new OAuth App (or a GitHub App for installation tokens).", "Set the Authorization callback URL to the redirect URI shown below.", "Select the repo/org scopes the agent needs.", "Paste the Client ID + Client secret below."}, "scopesRationale": "repo/read:org back the GitHub skills (PRs, issues, org-visible repos) as the connecting user."},
	},
	{
		Name: "x", Hosts: []string{"api.x.com"}, Scopes: []string{"tweet.read", "tweet.write", "users.read", "offline.access"}, ConsentMode: "standard", RedirectPath: "x/callback",
		SetupGuide: map[string]any{"console": "X Developer Portal → Projects & Apps → your app → User authentication settings", "url": "https://developer.x.com/en/portal/projects-and-apps", "steps": []string{"Set up User authentication: OAuth 2.0, type Confidential client, App permissions Read and write.", "Add the Callback URI shown below to the app's Callback URLs.", "Under Keys and tokens, generate the OAuth 2.0 Client ID and Client Secret.", "Paste the Client ID + Client secret below."}, "scopesRationale": "tweet.read/users.read back reads; tweet.write lets the connecting user post as themselves; offline.access issues a refresh token so the 2-hour access token renews."},
	},
}

func (h *HTTPServer) proxyOAuthCatalogRoute(r *http.Request) bool {
	if r.Method != http.MethodGet || (r.URL.Path != "/v1/connectors/catalog" && r.URL.Path != "/v1/connectors/oauth/status") {
		return false
	}
	return !h.config.QM.OAuthCatalogEnabled
}

func (h *HTTPServer) configuredOAuthProviders() map[string]bool {
	configured := make(map[string]bool, len(h.config.QM.OAuthConfiguredProviders))
	for _, provider := range h.config.QM.OAuthConfiguredProviders {
		configured[strings.TrimSpace(provider)] = true
	}
	return configured
}

func (h *HTTPServer) oauthCatalog(w http.ResponseWriter, _ *http.Request) {
	configured := h.configuredOAuthProviders()
	catalog := make([]map[string]any, 0, len(oauthCatalogProviders))
	for _, provider := range oauthCatalogProviders {
		catalog = append(catalog, map[string]any{
			"provider": provider.Name, "hosts": provider.Hosts, "scopes": provider.Scopes,
			"consentMode": provider.ConsentMode, "redirectPath": provider.RedirectPath,
			"setupGuide": provider.SetupGuide, "configured": configured[provider.Name],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalog": catalog})
}

func (h *HTTPServer) oauthStatus(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principalId"))
	if principalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "principalId required"})
		return
	}
	statuses, err := h.keychain.ConnectorTokenStatuses(r.Context(), principalID)
	if err != nil {
		h.fail(w, err)
		return
	}
	configured := h.configuredOAuthProviders()
	providers := make(map[string]any, len(oauthCatalogProviders))
	for _, provider := range oauthCatalogProviders {
		hosts := make([]map[string]any, 0, len(provider.Hosts))
		connected := false
		needsReconnect := false
		var latestFailure *int64
		latestError := ""
		for _, host := range provider.Hosts {
			best := bestOAuthConnectorStatus(statuses, principalID, host)
			hostView := map[string]any{"host": host, "connected": best.Connected}
			if best.ExpiresAt != nil {
				hostView["expiresAt"] = *best.ExpiresAt
			}
			if best.HasRefreshToken {
				hostView["hasRefreshToken"] = true
			}
			if best.NeedsReconnect {
				hostView["needsReconnect"] = true
			}
			if best.RefreshFailedAt != nil {
				hostView["refreshFailedAt"] = *best.RefreshFailedAt
			}
			if best.RefreshError != "" {
				hostView["refreshError"] = best.RefreshError
			}
			if best.AccountType != "" {
				hostView["accountType"] = best.AccountType
			}
			if len(best.GrantedScopes) != 0 {
				hostView["grantedScopes"] = best.GrantedScopes
			}
			hosts = append(hosts, hostView)
			connected = connected || best.Connected && !best.NeedsReconnect
			needsReconnect = needsReconnect || best.NeedsReconnect
			if best.RefreshFailedAt != nil && (latestFailure == nil || *best.RefreshFailedAt > *latestFailure) {
				value := *best.RefreshFailedAt
				latestFailure, latestError = &value, best.RefreshError
			}
		}
		view := map[string]any{"hosts": hosts, "connected": connected, "configured": configured[provider.Name], "available": configured[provider.Name], "consentMode": provider.ConsentMode}
		if needsReconnect {
			view["needsReconnect"] = true
		}
		if latestFailure != nil {
			view["refreshFailedAt"] = *latestFailure
		}
		if latestError != "" {
			view["refreshError"] = latestError
		}
		providers[provider.Name] = view
	}
	if err := h.audit.Record(r.Context(), data.AuditEvent{PrincipalID: principalID, Action: "connector.oauth.status", Resource: "connectors", ScopeLabel: principalID, IdempotencyKey: requestID(r)}); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principalId": principalID, "providers": providers})
}

func bestOAuthConnectorStatus(statuses []data.ConnectorTokenStatus, principalID, host string) data.ConnectorTokenStatus {
	candidates := make([]data.ConnectorTokenStatus, 0, 3)
	for _, status := range statuses {
		if !samePrincipal(status.OwnerID, principalID) || !strings.EqualFold(status.Host, host) {
			continue
		}
		candidates = append(candidates, status)
	}
	for _, accountType := range []string{"", "personal", "company"} {
		for _, candidate := range candidates {
			if candidate.AccountType == accountType && candidate.Connected && !candidate.NeedsReconnect {
				return candidate
			}
		}
	}
	for _, accountType := range []string{"", "personal", "company"} {
		for _, candidate := range candidates {
			if candidate.AccountType == accountType && candidate.Connected {
				return candidate
			}
		}
	}
	return data.ConnectorTokenStatus{}
}
