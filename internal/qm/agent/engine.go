package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/loon-hejw/knowlega/internal/qm/config"
)

type ControlTransport string
type ToolTransport string
type Capability string

const (
	CapabilityAbort            Capability = "abort"
	CapabilitySteer            Capability = "steer"
	CapabilityImages           Capability = "images"
	CapabilityThinkingLevel    Capability = "thinking-level"
	CapabilityFastMode         Capability = "fast-mode"
	CapabilityProviderSessions Capability = "provider-sessions"
)

type Profile struct {
	ID               string
	ControlTransport ControlTransport
	ToolTransport    ToolTransport
	TranscriptFormat string
	Capabilities     map[Capability]bool
}

type Choice struct {
	HarnessID string
	ModelID   string
}

type NewEntry struct {
	Type       string
	Payload    json.RawMessage
	ScopeLabel string
}

type SessionEntry struct {
	SessionID      string          `json:"sessionId"`
	Sequence       int             `json:"seq"`
	ParentSequence *int            `json:"parentSeq"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	ScopeLabel     string          `json:"scopeLabel"`
	CreatedAt      int64           `json:"createdAt"`
}

type ModelCallRecord struct {
	Model       string `json:"model"`
	InputTokens int    `json:"inputTokens"`
	EntryCount  int    `json:"entryCount"`
}

type LLMRequestRecord struct {
	TurnSequence                  *int            `json:"turnSeq"`
	Step                          int             `json:"step"`
	Model                         string          `json:"model"`
	Request                       json.RawMessage `json:"request"`
	Truncated                     bool            `json:"truncated"`
	TTFTMS, DurationMS, StepGapMS *int            `json:"-"`
	ToolWallMS, GapPhases, Usage  json.RawMessage `json:"-"`
	Transport                     json.RawMessage `json:"-"`
}

type ToolOptions struct {
	ReadOnly     bool
	SurfaceTools bool
	SurfaceName  string
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ToolResult struct {
	Content   []ToolContent   `json:"content"`
	Details   json.RawMessage `json:"details,omitempty"`
	IsError   bool            `json:"isError,omitempty"`
	Terminate bool            `json:"terminate,omitempty"`
	Silent    bool            `json:"silent,omitempty"`
}

type ToolContext interface {
	Definitions(context.Context, ToolOptions) ([]ToolDefinition, error)
	Execute(context.Context, ToolCall) (ToolResult, error)
}

type TurnSignal struct {
	Kind      string
	Text      string
	CreatedAt int64
	Payload   json.RawMessage
}

type TapeRecord struct {
	Kind, Harness, ScopeLabel          string
	Payload                            json.RawMessage
	BareText, Timestamp                string
	EntrySequence, CoversEntrySequence *int
	Sequence                           int
	CreatedAt                          int64
}

type TurnInput struct {
	SessionID           string
	RunID               string
	Input               string
	SystemPrompt        string
	SystemCacheBoundary *int
	ScopeLabel          string
	OrgScopeID          string
	Environment         string
	Model               string
	Harness             string
	ThinkingLevel       string
	FastMode            bool
	ReadOnly            bool
	SurfaceTools        bool
	SurfaceName         string
	PollFire            bool
	TurnWallClockMS     int
	History             json.RawMessage
	PriorTurns          json.RawMessage
	Attachments         json.RawMessage
	Images              []Image
	UserEntrySequence   *int
	Tools               ToolContext
	Emit                func(context.Context, NewEntry) (SessionEntry, error)
	FindToolResult      func(context.Context, string) (ToolResult, bool, error)
	ToolApprovalGate    func(string) bool
	RecordModelCall     func(ModelCallRecord)
	RecordLLMRequest    func(context.Context, LLMRequestRecord) error
	TakePendingSignals  func(context.Context, string) ([]TurnSignal, error)
	Tape                func(context.Context, TapeRecord) error
	TapeRows            []TapeRecord
	TapeMode            string
	OnDelta             func(string)
	OnTextBlockStart    func()
	OnProgress          func(Progress)
}

type Image struct {
	MIMEType   string `json:"mimeType"`
	DataBase64 string `json:"dataBase64"`
	ArtifactID string `json:"artifactId,omitempty"`
}

type Progress struct {
	Attempt       int    `json:"attempt,omitempty"`
	Model         string `json:"model,omitempty"`
	ToolCalls     int    `json:"toolCalls"`
	ModelCalls    int    `json:"modelCalls,omitempty"`
	EvidenceCount int    `json:"evidenceCount,omitempty"`
	ModelLimit    int    `json:"modelLimit,omitempty"`
	ToolLimit     int    `json:"toolLimit,omitempty"`
	Tokens        int    `json:"tokens,omitempty"`
	Step          int    `json:"step,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Strategy      string `json:"strategy,omitempty"`
	Stalled       bool   `json:"stalled,omitempty"`
}

type PendingApproval struct {
	RequestID   string `json:"requestId,omitempty"`
	Command     string `json:"command"`
	Reason      string `json:"reason"`
	Kind        string `json:"kind,omitempty"`
	Matched     string `json:"matched,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	ApprovalKey string `json:"approvalKey,omitempty"`
}

type CacheUsage struct {
	CacheRead     int `json:"cacheRead"`
	CacheWrite    int `json:"cacheWrite"`
	UncachedInput int `json:"uncachedInput"`
}

type TurnResult struct {
	Status             string            `json:"status,omitempty"`
	Reply              string            `json:"reply"`
	Silent             bool              `json:"silent,omitempty"`
	Stopped            bool              `json:"stopped,omitempty"`
	PendingApprovals   []PendingApproval `json:"pendingApprovals,omitempty"`
	PausedOnApproval   bool              `json:"pausedOnApproval,omitempty"`
	ModelCalls         int               `json:"modelCalls,omitempty"`
	ToolCalls          int               `json:"toolCalls,omitempty"`
	EvidenceCount      int               `json:"evidenceCount,omitempty"`
	CompletionStatus   string            `json:"completionStatus,omitempty"`
	Reason             string            `json:"reason,omitempty"`
	NextAction         string            `json:"nextAction,omitempty"`
	ModelLimit         int               `json:"modelLimit,omitempty"`
	ToolLimit          int               `json:"toolLimit,omitempty"`
	FallbackModel      string            `json:"fallbackModel,omitempty"`
	Partial            string            `json:"partial,omitempty"`
	CacheUsage         *CacheUsage       `json:"cacheUsage,omitempty"`
	CompileMS          int               `json:"compileMs,omitempty"`
	TapeWriteFailed    bool              `json:"tapeWriteFailed,omitempty"`
	FinalEntrySequence *int              `json:"-"`
}

type Adapter interface {
	Profile() Profile
	RunTurn(context.Context, TurnInput) (TurnResult, error)
	ResetSession(context.Context, string) error
	Close(context.Context) error
}

type UtilityInput struct {
	Harness, Model, ScopeLabel string
	SystemPrompt, Prompt       string
	RecordModelCall            func(ModelCallRecord)
	RecordLLMRequest           func(context.Context, LLMRequestRecord) error
}

type DetectInput struct {
	UtilityInput
	Message, RecentContext, ThreadOpener, ReactionGuidance string
	History                                                json.RawMessage
}

type DetectResult struct {
	Respond   bool     `json:"respond"`
	Reactions []string `json:"reactions,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

type CompactInput struct {
	UtilityInput
	History json.RawMessage
}

type SecurityVerdict struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

type ModelUtilityAdapter interface {
	ShouldRespond(context.Context, DetectInput) DetectResult
	CompactHistory(context.Context, CompactInput) string
	ContextTokenBudget(string) (int, bool)
	OneShot(context.Context, UtilityInput) (string, error)
	Judge(context.Context, UtilityInput) (string, error)
	ScreenSecurity(context.Context, UtilityInput) (*SecurityVerdict, error)
	PickAckEmoji(context.Context, UtilityInput, []string) (string, error)
	GenerateTitle(context.Context, UtilityInput) (string, error)
	SummarizeApproval(context.Context, UtilityInput, string, string, string) (string, error)
}

type SessionSelectionStore interface {
	Select(context.Context, string, string) (string, bool, error)
	Delete(context.Context, string) error
}

type ChoiceResolver interface {
	Resolve(context.Context, TurnInput, Choice) (Choice, error)
}

type ChoiceResolverFunc func(context.Context, TurnInput, Choice) (Choice, error)

func (f ChoiceResolverFunc) Resolve(ctx context.Context, input TurnInput, fallback Choice) (Choice, error) {
	return f(ctx, input, fallback)
}

type Engine struct {
	models   config.ModelsConfig
	adapters map[string]Adapter
	store    SessionSelectionStore
	resolver ChoiceResolver
}

func NewEngine(models config.ModelsConfig, store SessionSelectionStore, adapters []Adapter, resolver ChoiceResolver) (*Engine, error) {
	if store == nil {
		return nil, errors.New("agent harness session store is required")
	}
	engine := &Engine{models: models, store: store, resolver: resolver, adapters: map[string]Adapter{}}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil harness adapter")
		}
		profile := adapter.Profile()
		configured, ok := models.Harness(profile.ID)
		if !ok {
			return nil, fmt.Errorf("harness %s is not configured", profile.ID)
		}
		if !sameProfile(profile, Profiles()[profile.ID]) {
			return nil, fmt.Errorf("harness %s profile does not match the Node contract", profile.ID)
		}
		if configured.ID == "" || engine.adapters[profile.ID] != nil {
			return nil, fmt.Errorf("duplicate harness adapter %s", profile.ID)
		}
		engine.adapters[profile.ID] = adapter
	}
	return engine, nil
}

func (e *Engine) Resolve(ctx context.Context, input TurnInput) (Choice, error) {
	fallbackHarness, ok := e.models.Harness(e.models.DefaultHarness)
	if !ok {
		return Choice{}, &NonRetryableError{Err: errors.New("default harness is not configured")}
	}
	choice := Choice{HarnessID: fallbackHarness.ID, ModelID: fallbackHarness.DefaultModel}
	if e.resolver != nil {
		resolved, err := e.resolver.Resolve(ctx, input, choice)
		if err != nil {
			return Choice{}, err
		}
		choice = resolved
	}
	if input.Harness != "" {
		choice.HarnessID = input.Harness
	}
	if input.Model != "" {
		choice.ModelID = input.Model
	}
	harness, ok := e.models.Harness(choice.HarnessID)
	if !ok || !contains(harness.ModelIDs, choice.ModelID) {
		return Choice{}, &NonRetryableError{Err: fmt.Errorf("runtime %s/%s is not approved", choice.HarnessID, choice.ModelID)}
	}
	return choice, nil
}

func (e *Engine) RunTurn(ctx context.Context, input TurnInput) (TurnResult, error) {
	if strings.TrimSpace(input.SessionID) == "" {
		return TurnResult{}, errors.New("session id is required")
	}
	choice, err := e.Resolve(ctx, input)
	if err != nil {
		return TurnResult{}, err
	}
	adapter := e.adapters[choice.HarnessID]
	if adapter == nil {
		return TurnResult{}, &NonRetryableError{Err: fmt.Errorf("harness %s is unavailable", choice.HarnessID)}
	}
	previous, changed, err := e.store.Select(ctx, input.SessionID, choice.HarnessID)
	if err != nil {
		return TurnResult{}, err
	}
	if changed {
		if prior := e.adapters[previous]; prior != nil {
			if err := prior.ResetSession(ctx, input.SessionID); err != nil {
				return TurnResult{}, err
			}
		}
		if err := adapter.ResetSession(ctx, input.SessionID); err != nil {
			return TurnResult{}, err
		}
	}
	input.Harness = choice.HarnessID
	input.Model = choice.ModelID
	return adapter.RunTurn(ctx, input)
}

func (e *Engine) ResetSession(ctx context.Context, sessionID string) error {
	if err := e.store.Delete(ctx, sessionID); err != nil {
		return err
	}
	ids := make([]string, 0, len(e.adapters))
	for id := range e.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := e.adapters[id].ResetSession(ctx, sessionID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Close(ctx context.Context) error {
	ids := make([]string, 0, len(e.adapters))
	for id := range e.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := e.adapters[id].Close(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ModelUtilities(ctx context.Context, input UtilityInput) (ModelUtilityAdapter, UtilityInput, error) {
	choice, err := e.Resolve(ctx, TurnInput{SessionID: "model-utility", Harness: input.Harness, Model: input.Model, ScopeLabel: input.ScopeLabel})
	if err != nil {
		return nil, input, err
	}
	adapter, ok := e.adapters[choice.HarnessID].(ModelUtilityAdapter)
	if !ok {
		return nil, input, &NonRetryableError{Err: fmt.Errorf("harness %s does not implement model utilities", choice.HarnessID)}
	}
	input.Harness = choice.HarnessID
	input.Model = choice.ModelID
	return adapter, input, nil
}

func (e *Engine) OneShot(ctx context.Context, input UtilityInput) (string, error) {
	adapter, resolved, err := e.ModelUtilities(ctx, input)
	if err != nil {
		return "", err
	}
	return adapter.OneShot(ctx, resolved)
}

func (e *Engine) ClassifyKnowledge(ctx context.Context, input UtilityInput) (string, error) {
	adapter, resolved, err := e.ModelUtilities(ctx, input)
	if err != nil {
		return "", err
	}
	if harness, ok := e.models.Harness(resolved.Harness); ok && strings.TrimSpace(harness.Runtime.DetectModel) != "" {
		resolved.Model = strings.TrimSpace(harness.Runtime.DetectModel)
	}
	return adapter.OneShot(ctx, resolved)
}

func (e *Engine) Judge(ctx context.Context, input UtilityInput) (string, error) {
	adapter, resolved, err := e.ModelUtilities(ctx, input)
	if err != nil {
		return "", err
	}
	return adapter.Judge(ctx, resolved)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func Profiles() map[string]Profile {
	return map[string]Profile{
		"pi":       profile("pi", "in-process", "in-process", "pi", CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityThinkingLevel, CapabilityFastMode, CapabilityProviderSessions),
		"opencode": profile("opencode", "http", "plugin", "opencode", CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityProviderSessions),
		"codex":    profile("codex", "json-rpc", "dynamic", "responses-api", CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityProviderSessions),
		"claude":   profile("claude", "sdk", "in-process-mcp", "claude-agent-sdk", CapabilityAbort, CapabilitySteer, CapabilityImages, CapabilityThinkingLevel, CapabilityFastMode),
		"mock":     profile("mock", "mock", "mock", "qm"),
	}
}

func profile(id string, control ControlTransport, tools ToolTransport, transcript string, capabilities ...Capability) Profile {
	set := make(map[Capability]bool, len(capabilities))
	for _, capability := range capabilities {
		set[capability] = true
	}
	return Profile{ID: id, ControlTransport: control, ToolTransport: tools, TranscriptFormat: transcript, Capabilities: set}
}

func sameProfile(left, right Profile) bool {
	if left.ID != right.ID || left.ControlTransport != right.ControlTransport || left.ToolTransport != right.ToolTransport || left.TranscriptFormat != right.TranscriptFormat || len(left.Capabilities) != len(right.Capabilities) {
		return false
	}
	for capability := range left.Capabilities {
		if !right.Capabilities[capability] {
			return false
		}
	}
	return true
}
