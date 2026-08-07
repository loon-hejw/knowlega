package server

import "github.com/hejw/qm-backend/internal/auth"

// agentAPIEndpoint is the public, token-scoped discovery record returned by
// GET /v1/apis. It deliberately describes both Go-owned endpoints and the
// remaining Node adapters: callers keep one stable control-plane contract as
// ownership moves behind the same URL.
type agentAPIEndpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

type agentAPIListing struct {
	ActorID   string             `json:"actorId"`
	ScopeID   string             `json:"scopeId"`
	Admin     map[string]any     `json:"admin"`
	Endpoints []agentAPIEndpoint `json:"endpoints"`
	Guidance  []string           `json:"guidance"`
}

func agentAPI(method, path, summary string) agentAPIEndpoint {
	return agentAPIEndpoint{Method: method, Path: path, Summary: summary}
}

func baseAgentAPIEndpoints() ([]agentAPIEndpoint, []string) {
	endpoints := []agentAPIEndpoint{
		agentAPI("GET", "/v1/apis", "this list — every endpoint your token can call right now, plus whether the user is an org admin"),
		agentAPI("GET", "/v1/runtime-config", "read this scope's effective harness/model, approved choices, override, and whether the org recommends an upgrade"),
		agentAPI("PUT", "/v1/runtime-config", "set this scope's default, follow the org with {inherit:true}, or acknowledge the recommendation with {keep:true}"),
		agentAPI("GET", "/v1/projects", "list every project the asking person belongs to"),
		agentAPI("POST", "/v1/projects", "create a project owned by the asking person — body {name}"),
		agentAPI("PATCH", "/v1/projects/:id", "rename a project the asking person owns — body {name}"),
		agentAPI("POST", "/v1/projects/:id/members", "add an internal directory member to a project the asking person belongs to — body {memberId}"),
		agentAPI("DELETE", "/v1/projects/:id/members/:memberId", "remove a member from a project the asking person owns"),
		agentAPI("POST", "/v1/triggers/:id/consent", "accept or decline a standing trigger's deliveries to you — body {decision:\"accept\"|\"decline\"}; recipient-only"),
		agentAPI("GET", "/v1/conversations", "list the asking person's own conversations — the same list their web sidebar shows"),
		agentAPI("POST", "/v1/conversations/:id", "update one of the asking person's conversations; archive, pin, rename, or set the sidebar color"),
		agentAPI("GET", "/v1/conversations/:id?tailTurns=20", "read a bounded transcript of one of the asking person's conversations"),
		agentAPI("POST", "/v1/conversations", "start a fresh conversation in this scope — body {text, title?}"),
		agentAPI("POST", "/v1/conversations/:id/fork", "fork one of the asking person's conversations — body optionally {upToSeq}"),
		agentAPI("GET", "/v1/files", "list files the asking person can reach across their contexts, split into owned and shared files"),
		agentAPI("GET", "/v1/files/:id/content", "download bytes of a file from the asking person's file library"),
		agentAPI("POST", "/v1/reach", "send a teammate a DM, post to a channel, or post to a group DM right now; choose the narrowest explicit audience"),
		agentAPI("GET", "/v1/deployments", "list published apps the asking person can reach across scopes"),
		agentAPI("GET", "/v1/deployments/:id", "inspect one deployment the asking person can reach"),
		agentAPI("GET", "/v1/deployments/:id/fetch", "read a deployment's rendered content as the asking person"),
		agentAPI("GET", "/v1/deployments/:id/git-url", "get an authenticated Git remote URL for a deployment the asking person can reach"),
		agentAPI("POST", "/v1/deployments/:id/share", "change who can reach a published app you own; target one scope or recipient and choose access"),
		agentAPI("POST", "/v1/deployments/:id/name", "rename an app you manage — body {name}"),
		agentAPI("POST", "/v1/deployments/:id/display-name", "set an app's human-friendly display name — body {displayName}"),
		agentAPI("POST", "/v1/deployments/:id/archive", "take down an app you manage while preserving its source"),
		agentAPI("POST", "/v1/deployments/:id/restore", "restore an archived app you manage"),
		agentAPI("POST|GET", "/v1/keychain/credentials", "register a login to the user's keychain or list credential metadata"),
		agentAPI("GET", "/v1/keychain/overview", "list credential metadata, grants, pending asks, and recent audited use; never secret values"),
		agentAPI("DELETE", "/v1/keychain/credentials/:id", "remove a registered login"),
		agentAPI("POST|GET", "/v1/keychain/grants", "request a purpose-bound credential grant or list grants"),
		agentAPI("POST", "/v1/keychain/grants/:id/revoke", "revoke a credential grant"),
		agentAPI("POST|GET", "/v1/keychain/asks", "ask a credential owner for access or list asks"),
		agentAPI("POST", "/v1/keychain/asks/:id/decline", "decline a credential ask"),
		agentAPI("POST", "/v1/keychain/drops", "mint a single-use, expiring browser link for someone to drop a credential; pass the returned URL verbatim"),
		agentAPI("POST", "/v1/keychain/use", "materialize an approved credential grant into environment variables for this turn"),
		agentAPI("POST", "/v1/surface-context", "fetch recent messages from a channel or DM the asking person can see"),
		agentAPI("POST", "/v1/surface-file", "fetch a file posted in a channel or DM the asking person can see"),
		agentAPI("GET", "/v1/environments", "list the organization's named environments and attached scopes"),
		agentAPI("POST", "/v1/environments", "promote this conversation's computer to a named environment"),
		agentAPI("POST", "/v1/environments/attach", "attach this conversation to a named environment"),
		agentAPI("POST", "/v1/skills", "save a new skill in this conversation's scope — {name, description, body}"),
		agentAPI("GET", "/v1/skills/:id", "read a skill you can see, including its body, files, status, and version"),
		agentAPI("PUT", "/v1/skills/:id", "edit a skill you manage"),
		agentAPI("DELETE", "/v1/skills/:id", "archive a skill you manage"),
		agentAPI("POST", "/v1/skills/:id/restore", "restore an archived skill you manage"),
		agentAPI("POST", "/v1/emoji", "add one Slack custom emoji — {name, image:<base64 PNG/GIF>}"),
		agentAPI("POST", "/v1/connectors/oauth/revoke", "disconnect an OAuth connector; confirm before revoking another participant's connector"),
	}
	guidance := []string{
		"Runtime choice is scoped: changing it affects this personal or shared context, not the whole org. Confirm before changing a shared scope. An inherit reset follows future org defaults automatically.",
		"These act as the asking person across every project they belong to. Do not send principalId; the capability token determines the person, and inaccessible projects return 404.",
		"If a teammate set up a recurring delivery, its recipient controls whether it reaches them. Only the recipient can decide.",
		"These act on the asking person's own conversation list. Confirm before bulk-archiving.",
		"These show the asking person's file library across every context they can reach. A file outside visibility returns 404 without revealing whether it exists.",
		"To see published apps you can reach, use deployments. To widen or narrow access — share it with everyone or a teammate — use the deployment share endpoint; no redeploy.",
		"The keychain ask→approve→use protocol is documented in your keychain manifest when one renders.",
		"Save a skill when you have a repeatable procedure worth keeping. Skills load on future turns in their owning scope.",
		"Pick or produce a square image under roughly 128KB, base64 it, then call the emoji endpoint once.",
		"Disconnecting an OAuth connector is reversible by reconnecting; confirm before revoking someone else's.",
	}
	return endpoints, guidance
}

func memoryAgentAPIEndpoints() ([]agentAPIEndpoint, []string) {
	return []agentAPIEndpoint{
		agentAPI("POST", "/v1/memory/search", "search every notebook this conversation may read"),
		agentAPI("POST", "/v1/memory/facts", "append durable facts to this conversation's notebook"),
		agentAPI("GET|PUT", "/v1/memory/self", "read or rewrite this conversation's whole notebook; rewriting is destructive"),
		agentAPI("GET", "/v1/memory/history", "list notebook versions available to undo a rewrite"),
		agentAPI("POST", "/v1/memory/restore", "restore a prior notebook version using revision and expectedRevision"),
	}, []string{"Memory bodies and curation rules are documented in the memory skill."}
}

func adminAgentAPIEndpoints() []agentAPIEndpoint {
	return []agentAPIEndpoint{
		agentAPI("GET", "/v1/admin/whoami", "this user's admin status"),
		agentAPI("GET", "/v1/admin/scopes", "every scope with labels and what lives there"),
		agentAPI("GET", "/v1/admin/scopes/:scopeId", "a scope's resolved config"),
		agentAPI("PUT", "/v1/admin/scopes/:scopeId/:resource", "govern a scope's configuration resource"),
		agentAPI("GET|PUT", "/v1/admin/memory?scope=", "read or rewrite any scope's memory notebook"),
		agentAPI("GET", "/v1/admin/sessions?scope=", "conversation metadata; detailed transcript and LLM audit reads are available by id"),
		agentAPI("GET", "/v1/admin/runs?scope=", "queued, in-flight, and recent runs"),
		agentAPI("GET", "/v1/admin/files?scope=", "document-store listing and content reads"),
		agentAPI("GET", "/v1/admin/volumes?scope=", "a scope's computer and backup contents"),
		agentAPI("GET", "/v1/admin/crons|deployments|skills?scope=", "artifacts by owning scope"),
		agentAPI("GET", "/v1/admin/audit|errors|metrics|egress?scope=", "observability logs and telemetry"),
		agentAPI("GET", "/v1/admin/retention", "organization-wide usage and retention report"),
		agentAPI("GET", "/v1/admin/users", "organization roster with admin status"),
		agentAPI("GET", "/v1/admin/directory?q=", "resolve a name or principal id to organization-directory candidates"),
		agentAPI("GET", "/v1/admin/keychain", "person-owned keychain metadata, grants, and asks"),
	}
}

func agentAPIInsertionIndex(endpoints []agentAPIEndpoint, path string) int {
	for index, endpoint := range endpoints {
		if endpoint.Path == path {
			return index
		}
	}
	return len(endpoints)
}

func agentAPIGuidanceInsertionIndex(guidance []string, prefix string) int {
	for index, entry := range guidance {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			return index
		}
	}
	return len(guidance)
}

func insertAgentAPIEndpoints(endpoints, additions []agentAPIEndpoint, index int) []agentAPIEndpoint {
	result := make([]agentAPIEndpoint, 0, len(endpoints)+len(additions))
	result = append(result, endpoints[:index]...)
	result = append(result, additions...)
	return append(result, endpoints[index:]...)
}

func insertAgentAPIGuidance(guidance, additions []string, index int) []string {
	result := make([]string, 0, len(guidance)+len(additions))
	result = append(result, guidance[:index]...)
	result = append(result, additions...)
	return append(result, guidance[index:]...)
}

func renderAgentAPIs(identity auth.Identity, isAdmin bool, role string) agentAPIListing {
	endpoints, guidance := baseAgentAPIEndpoints()
	if identity.Memory != nil {
		memoryEndpoints, memoryGuidance := memoryAgentAPIEndpoints()
		if identity.Memory.OrgWrite != "" {
			memoryEndpoints = append(memoryEndpoints, agentAPI("POST|PUT", "/v1/memory/facts | /v1/memory/self", "add \"scope\":\"org\" to target the organization-wide notebook; confirm wording first"))
		}
		endpoints = insertAgentAPIEndpoints(endpoints, memoryEndpoints, agentAPIInsertionIndex(endpoints, "/v1/keychain/credentials"))
		guidance = insertAgentAPIGuidance(guidance, memoryGuidance, agentAPIGuidanceInsertionIndex(guidance, "The keychain"))
	}
	admin := map[string]any{"isAdmin": isAdmin}
	if role != "" {
		admin["role"] = role
	}
	if isAdmin && identity.LiveActor {
		endpoints = append(endpoints, adminAgentAPIEndpoints()...)
		guidance = append(guidance, "Admin plane: you act as this organization admin — live-authorized per call and audited under their name; confirm before any mutation. Admin grant changes remain portal-only.")
	} else {
		endpoints = append(endpoints, agentAPI("GET", "/v1/admin/whoami", "this user's organization capabilities: {permissions, isAdmin, role?}"))
	}
	return agentAPIListing{ActorID: identity.ActorID, ScopeID: identity.ScopeID, Admin: admin, Endpoints: endpoints, Guidance: guidance}
}
