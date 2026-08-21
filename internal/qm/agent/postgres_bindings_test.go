package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loon-hejw/knowlega/internal/qm/data"
)

type bindingSessionStore struct{}

func (bindingSessionStore) AppendEntry(context.Context, string, data.NewSessionEntry) (data.SessionEntry, error) {
	return data.SessionEntry{}, nil
}

func (bindingSessionStore) Entries(context.Context, string, int, int) ([]data.SessionEntry, error) {
	return nil, nil
}
func (bindingSessionStore) RecordLLMRequest(context.Context, string, data.NewLLMRequest) (data.LLMRequest, error) {
	return data.LLMRequest{}, nil
}
func (bindingSessionStore) AppendTape(context.Context, string, data.NewTapeRecord) (data.TapeRecord, error) {
	return data.TapeRecord{}, nil
}
func (bindingSessionStore) Tape(context.Context, string, int) ([]data.TapeRecord, error) {
	return nil, nil
}
func (bindingSessionStore) TapeCoverage(context.Context, string) (int, error) { return -1, nil }

type strictToolPolicy struct{ durable []string }

func (p strictToolPolicy) ToolApprovalPolicy(context.Context, string, string, string, string) (bool, []string, error) {
	return true, p.durable, nil
}

type capturedApprovalStore struct {
	values []data.AgentPendingApprovalInput
}

func (s *capturedApprovalStore) PutPending(_ context.Context, input data.AgentPendingApprovalInput) error {
	s.values = append(s.values, input)
	return nil
}

func TestPostgresBindingsStrictApprovalConsumesOnceAndKeepsDurableGrant(t *testing.T) {
	approvals := &capturedApprovalStore{}
	resolver := NewPostgresBindingsResolver(bindingSessionStore{}, nil).WithApprovals(approvals, strictToolPolicy{durable: []string{"tool:read"}})
	bindings, err := resolver.ResolveTurnBindings(context.Background(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", ActorID: "alice", ApprovedToolKeys: []string{"tool:write"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bindings.ToolApprovalGate("read") || !bindings.ToolApprovalGate("read") || !bindings.ToolApprovalGate("write") || bindings.ToolApprovalGate("write") || bindings.ToolApprovalGate("execute") {
		t.Fatal("strict approval gate did not preserve durable and single-use semantics")
	}
	if err := bindings.PersistApprovals(context.Background(), []PendingApproval{{RequestID: "p1", Command: "execute", ApprovalKey: "tool:execute"}}); err != nil {
		t.Fatal(err)
	}
	if len(approvals.values) != 1 || approvals.values[0].ApprovalKey != "tool:execute" {
		t.Fatalf("approvals=%#v", approvals.values)
	}
}

func TestPostgresBindingsPreparesInboundAttachments(t *testing.T) {
	materializer := &fakeInboundMaterializer{result: MaterializedInbound{Attachments: json.RawMessage(`[{"name":"a.txt"}]`), Environment: "<environment>file</environment>", Images: []Image{{MIMEType: "image/png", DataBase64: "eA=="}}}}
	resolver := NewPostgresBindingsResolver(bindingSessionStore{}, nil).WithInboundMaterializer(materializer)
	bindings, err := resolver.ResolveTurnBindings(context.Background(), TurnTaskPayload{SessionID: "s1", ScopeLabel: "personal:alice", OrgScopeID: "org:acme", WorkspaceKey: "personal:alice", Attachments: json.RawMessage(`[{"blobId":"abc"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	input := TurnInput{Environment: "existing"}
	if _, err := bindings.PrepareInput(context.Background(), &input); err != nil {
		t.Fatal(err)
	}
	if materializer.workspaceKey != "personal:alice" || len(input.Images) != 1 || !stringsContains(input.Environment, "existing", "<environment>file</environment>") {
		t.Fatalf("workspace=%q input=%#v", materializer.workspaceKey, input)
	}
}

type fakeInboundMaterializer struct {
	workspaceKey string
	result       MaterializedInbound
}

func (m *fakeInboundMaterializer) Materialize(_ context.Context, workspaceKey string, _ json.RawMessage) (MaterializedInbound, error) {
	m.workspaceKey = workspaceKey
	return m.result, nil
}

func stringsContains(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
