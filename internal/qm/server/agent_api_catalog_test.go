package server

import (
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
)

func hasAgentAPIPath(endpoints []agentAPIEndpoint, path string) bool {
	for _, endpoint := range endpoints {
		if endpoint.Path == path {
			return true
		}
	}
	return false
}

func agentAPIPathCount(endpoints []agentAPIEndpoint, path string) int {
	count := 0
	for _, endpoint := range endpoints {
		if endpoint.Path == path {
			count++
		}
	}
	return count
}

func agentAPIPathIndex(endpoints []agentAPIEndpoint, path string) int {
	for index, endpoint := range endpoints {
		if endpoint.Path == path {
			return index
		}
	}
	return -1
}

func TestRenderAgentAPIsCapabilityVisibility(t *testing.T) {
	regular := renderAgentAPIs(auth.Identity{ActorID: "U1", ScopeID: "personal:U1"}, false, "")
	if regular.ActorID != "U1" || regular.ScopeID != "personal:U1" || regular.Admin["isAdmin"] != false {
		t.Fatalf("regular listing=%#v", regular)
	}
	for _, path := range []string{"/v1/apis", "/v1/projects", "/v1/skills", "/v1/keychain/overview", "/v1/admin/whoami"} {
		if !hasAgentAPIPath(regular.Endpoints, path) {
			t.Fatalf("regular listing missing %s", path)
		}
	}
	for _, path := range []string{"/v1/admin/scopes", "/v1/memory/self", "/v1/crons", "/v1/soul"} {
		if hasAgentAPIPath(regular.Endpoints, path) {
			t.Fatalf("regular listing unexpectedly exposes %s", path)
		}
	}

	memory := renderAgentAPIs(auth.Identity{ActorID: "U1", ScopeID: "personal:U1", Memory: &auth.MemoryGrant{Write: "personal:U1", Read: []string{"personal:U1"}}}, false, "")
	if !hasAgentAPIPath(memory.Endpoints, "/v1/memory/self") || hasAgentAPIPath(memory.Endpoints, "/v1/memory/facts | /v1/memory/self") {
		t.Fatalf("personal memory listing=%#v", memory.Endpoints)
	}
	memoryOrg := renderAgentAPIs(auth.Identity{ActorID: "U1", ScopeID: "personal:U1", Memory: &auth.MemoryGrant{Write: "personal:U1", OrgWrite: "org:acme", Read: []string{"personal:U1", "org:acme"}}}, false, "")
	if !hasAgentAPIPath(memoryOrg.Endpoints, "/v1/memory/facts | /v1/memory/self") {
		t.Fatalf("org memory listing=%#v", memoryOrg.Endpoints)
	}
	if memoryIndex, keychainIndex := agentAPIPathIndex(memoryOrg.Endpoints, "/v1/memory/self"), agentAPIPathIndex(memoryOrg.Endpoints, "/v1/keychain/credentials"); memoryIndex < 0 || keychainIndex < 0 || memoryIndex > keychainIndex {
		t.Fatalf("memory endpoint order differs from Node catalog: memory=%d keychain=%d", memoryIndex, keychainIndex)
	}

	admin := renderAgentAPIs(auth.Identity{ActorID: "admin", ScopeID: "personal:admin", LiveActor: true}, true, "org_admin")
	if admin.Admin["role"] != "org_admin" || !hasAgentAPIPath(admin.Endpoints, "/v1/admin/scopes") || agentAPIPathCount(admin.Endpoints, "/v1/admin/whoami") != 1 {
		t.Fatalf("admin listing=%#v", admin)
	}
	autonomousAdmin := renderAgentAPIs(auth.Identity{ActorID: "admin", ScopeID: "personal:admin"}, true, "org_admin")
	if hasAgentAPIPath(autonomousAdmin.Endpoints, "/v1/admin/scopes") || agentAPIPathCount(autonomousAdmin.Endpoints, "/v1/admin/whoami") != 1 {
		t.Fatalf("autonomous admin listing=%#v", autonomousAdmin)
	}
}
