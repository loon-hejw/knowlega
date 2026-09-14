package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	knowlega "github.com/loon-hejw/knowlega/internal/agent/knowlega"
	knowledgecore "github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	knowledgeservice "github.com/loon-hejw/knowlega/internal/agent/knowlega/service"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

const toolResultLimit = 100000
const knowledgeDocumentContentLimit = 16000

type MemoryToolStore interface {
	Head(context.Context, string) (data.MemoryHead, error)
	Replace(context.Context, string, string, string) error
}

type SessionToolStore interface {
	Entries(context.Context, string, int, int) ([]data.SessionEntry, error)
}

type KnowledgeToolStore interface {
	Status(knowlega.ScopeRef) (knowlega.ScopeStatus, error)
	Search(context.Context, knowlega.ScopeRef, string, int) ([]knowledgecore.KnowledgeSearchResult, error)
	Discover(context.Context, knowlega.ScopeRef, []knowledgecore.KnowledgeRequirement, int) ([]knowledgecore.KnowledgeCandidate, error)
	Read(knowlega.ScopeRef, string) (knowledgeservice.KnowledgeDocument, error)
	List(knowlega.ScopeRef, string, int) ([]knowledgeservice.KnowledgePage, error)
	FollowLinks(knowlega.ScopeRef, string, int) (knowledgeservice.KnowledgeFollowResult, error)
	Graph(context.Context, knowlega.ScopeRef, string, []string, int) ([]knowledgeservice.KnowledgeDocument, error)
	Writeback(context.Context, knowlega.ScopeRef, string, knowledgecore.KnowledgeSubmission) (knowledgeservice.KnowledgeWritebackResult, error)
}

type CoreToolContextResolver struct {
	memory        MemoryToolStore
	sessions      SessionToolStore
	knowledge     KnowledgeToolStore
	workspaceRoot string
	sandboxTasks  SandboxTaskStore
}

type CoreToolContextOptions struct {
	Memory        MemoryToolStore
	Sessions      SessionToolStore
	Knowledge     KnowledgeToolStore
	WorkspaceRoot string
	SandboxTasks  SandboxTaskStore
}

func NewCoreToolContextResolver(options CoreToolContextOptions) (*CoreToolContextResolver, error) {
	root := strings.TrimSpace(options.WorkspaceRoot)
	if root == "" {
		return nil, errors.New("agent workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &CoreToolContextResolver{memory: options.Memory, sessions: options.Sessions, knowledge: options.Knowledge, workspaceRoot: abs, sandboxTasks: options.SandboxTasks}, nil
}

func (r *CoreToolContextResolver) ResolveToolContext(_ context.Context, payload TurnTaskPayload) (ToolContext, error) {
	if r == nil || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.ScopeLabel) == "" {
		return nil, errors.New("complete turn scope is required for tools")
	}
	workspaceKey := payload.WorkspaceKey
	if strings.TrimSpace(workspaceKey) == "" {
		workspaceKey = payload.ScopeLabel
	}
	return &coreToolContext{
		memory: r.memory, sessions: r.sessions, knowledge: r.knowledge, workspaceRoot: r.workspaceRoot,
		workspaceKey: workspaceKey, sessionID: payload.SessionID, scopeLabel: payload.ScopeLabel,
		orgScopeID: payload.OrgScopeID, runID: payload.RunID, modelID: payload.Model, harnessID: payload.Harness,
		readOnly: payload.ReadOnly, turnStartSeq: valueOrZero(payload.UserEntrySequence),
		turnStartKnown: payload.UserEntrySequence != nil, sandboxTasks: r.sandboxTasks,
	}, nil
}

type coreToolContext struct {
	memory         MemoryToolStore
	sessions       SessionToolStore
	knowledge      KnowledgeToolStore
	workspaceRoot  string
	workspaceKey   string
	sessionID      string
	scopeLabel     string
	orgScopeID     string
	runID          string
	modelID        string
	harnessID      string
	readOnly       bool
	turnStartSeq   int
	turnStartKnown bool
	sandboxTasks   SandboxTaskStore
	emit           func(context.Context, NewEntry) (SessionEntry, error)
}

func (t *coreToolContext) BindProgressEmitter(emit func(context.Context, NewEntry) (SessionEntry, error)) {
	t.emit = emit
}

func (t *coreToolContext) Definitions(_ context.Context, options ToolOptions) ([]ToolDefinition, error) {
	definitions := coreToolDefinitions(t.knowledge != nil && isProjectKnowledgeScope(t.scopeLabel))
	if t.readOnly || options.ReadOnly {
		allowed := map[string]bool{
			"memory": true, "history": true, "knowledge": true,
		}
		filtered := definitions[:0]
		for _, definition := range definitions {
			if allowed[definition.Name] {
				filtered = append(filtered, definition)
			}
		}
		definitions = filtered
	}
	return definitions, nil
}

func (t *coreToolContext) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	switch call.Name {
	case "execute":
		return t.executeSandbox(ctx, call)
	case "publish":
		return toolError("[publish unavailable] the Go deployment runtime is not configured"), nil
	case "background":
		return toolError("[background unavailable] the Go background process broker is not configured"), nil
	case "read":
		return t.executeRead(call.Arguments)
	case "write":
		return t.executeWrite(call.Arguments)
	case "memory":
		return t.executeMemory(ctx, call.Arguments)
	case "history":
		return t.executeHistory(ctx, call.Arguments)
	case "knowledge":
		return t.executeKnowledge(ctx, call, call.Arguments)
	default:
		return ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (t *coreToolContext) executeRead(raw json.RawMessage) (ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := decodeToolArguments(raw, &input); err != nil || strings.TrimSpace(input.Path) == "" {
		return toolError("[error] read requires `path`."), nil
	}
	path, err := t.workspacePath(input.Path, false)
	if err != nil {
		return toolError("[read failed] " + err.Error()), nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return toolText("[no such file: " + input.Path + "]"), nil
	}
	if err != nil {
		return toolError("[read failed] " + err.Error()), nil
	}
	return toolText(capToolText(string(content))), nil
}

func (t *coreToolContext) executeWrite(raw json.RawMessage) (ToolResult, error) {
	if t.readOnly {
		return toolError("[this is a read-only wake — workspace files cannot be written here]"), nil
	}
	var input struct {
		Path  string           `json:"path"`
		Data  *string          `json:"data"`
		Share []map[string]any `json:"share"`
	}
	if err := decodeToolArguments(raw, &input); err != nil || strings.TrimSpace(input.Path) == "" {
		return toolError("[error] write requires `path`."), nil
	}
	if len(input.Share) > 0 {
		return toolError("[write failed] Go workspace ACL sharing is not configured"), nil
	}
	if input.Data == nil {
		return toolText("nothing to do for " + input.Path), nil
	}
	path, err := t.workspacePath(input.Path, true)
	if err != nil {
		return toolError("[write failed] " + err.Error()), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return ToolResult{}, err
	}
	if err := os.WriteFile(path, []byte(*input.Data), 0600); err != nil {
		return ToolResult{}, err
	}
	return toolText(fmt.Sprintf("wrote %s (%d bytes)", input.Path, len(*input.Data))), nil
}

func (t *coreToolContext) executeMemory(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	if t.memory == nil {
		return toolError("[memory isn't available in this conversation]"), nil
	}
	store := t.memory
	var input struct {
		Action  string   `json:"action"`
		Query   string   `json:"query"`
		Limit   int      `json:"limit"`
		Facts   []string `json:"facts"`
		Content *string  `json:"content"`
	}
	if err := decodeToolArguments(raw, &input); err != nil {
		return toolError("[error] invalid memory arguments."), nil
	}
	head, err := store.Head(ctx, t.scopeLabel)
	if err != nil {
		return ToolResult{}, err
	}
	switch input.Action {
	case "search":
		if strings.TrimSpace(input.Query) == "" {
			return toolError("[error] memory search requires `query`."), nil
		}
		limit := boundedLimit(input.Limit, 20, 100)
		hits := memoryQuery(head.Content, input.Query, limit)
		if len(hits) == 0 {
			return toolText(fmt.Sprintf("[no remembered facts match %q]", input.Query)), nil
		}
		return toolText("- " + strings.Join(hits, "\n- ")), nil
	case "read":
		if strings.TrimSpace(head.Content) == "" {
			return toolText("(you have nothing remembered here yet)"), nil
		}
		return toolText(capToolText(head.Content)), nil
	case "remember":
		if t.readOnly {
			return toolError("[this is a read-only wake — memory can be searched and read here, but not written]"), nil
		}
		next, added := foldMemoryFacts(head.Content, input.Facts, time.Now())
		if len(input.Facts) == 0 || added == 0 && allBlank(input.Facts) {
			return toolError("[error] memory remember requires `facts` (a non-empty list)."), nil
		}
		if added > 0 {
			if err := store.Replace(ctx, t.scopeLabel, next, "agent:"+t.sessionID); err != nil {
				return ToolResult{}, err
			}
		}
		if added == 0 {
			return toolText("Already remembered — nothing new to save."), nil
		}
		return toolText(fmt.Sprintf("Remembered %d fact%s.", added, pluralSuffix(added))), nil
	case "rewrite":
		if t.readOnly {
			return toolError("[this is a read-only wake — memory can be searched and read here, but not written]"), nil
		}
		if input.Content == nil {
			return toolError("[error] memory rewrite requires `content` (the full new notebook)."), nil
		}
		if err := store.Replace(ctx, t.scopeLabel, *input.Content, "agent:"+t.sessionID); err != nil {
			return ToolResult{}, err
		}
		return toolText("Rewrote your memory notebook."), nil
	default:
		return toolError("[error] memory action must be search, remember, read, or rewrite."), nil
	}
}

func (t *coreToolContext) executeHistory(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	if t.sessions == nil {
		return toolText("[]"), nil
	}
	store := t.sessions
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeToolArguments(raw, &input); err != nil || strings.TrimSpace(input.Query) == "" {
		return toolError("[error] history requires `query`."), nil
	}
	entries, err := store.Entries(ctx, t.sessionID, 0, 0)
	if err != nil {
		return ToolResult{}, err
	}
	hits := searchHistory(entries, input.Query, boundedLimit(input.Limit, 20, 100))
	if len(hits) == 0 {
		return toolText(fmt.Sprintf("[nothing in this conversation's transcript matches %q]", input.Query)), nil
	}
	return toolText("- " + strings.Join(hits, "\n- ")), nil
}

func (t *coreToolContext) executeKnowledge(ctx context.Context, call ToolCall, raw json.RawMessage) (ToolResult, error) {
	if !isProjectKnowledgeScope(t.scopeLabel) {
		return knowledgeUnavailable("Project Knowledge is only available in project conversations"), nil
	}
	if t.knowledge == nil {
		return knowledgeUnavailable("Knowledge Core is not configured for this scope"), nil
	}
	var input struct {
		Action       string                                 `json:"action"`
		SearchScope  string                                 `json:"scope"`
		Query        string                                 `json:"query"`
		Path         string                                 `json:"path"`
		Limit        int                                    `json:"limit"`
		SeedPaths    []string                               `json:"seed_paths"`
		Requirements []knowledgecore.KnowledgeRequirement   `json:"requirements"`
		Question     string                                 `json:"question"`
		Answer       string                                 `json:"answer"`
		Candidate    string                                 `json:"candidate"`
		Checks       []knowledgecore.KnowledgeEvidenceCheck `json:"checks"`
		Evidence     []string                               `json:"evidence_paths"`
		Status       string                                 `json:"status"`
		Title        string                                 `json:"title"`
		Submission   *knowledgecore.KnowledgeSubmission     `json:"submission"`
		Requirement  string                                 `json:"requirement_id"`
	}
	if err := decodeToolArguments(raw, &input); err != nil {
		return toolError("[error] invalid knowledge arguments."), nil
	}
	input.Action = strings.ToLower(strings.TrimSpace(input.Action))
	input.Query = strings.TrimSpace(input.Query)
	input.Path = strings.TrimSpace(input.Path)
	input.Candidate = strings.TrimSpace(input.Candidate)
	input.Requirement = strings.TrimSpace(input.Requirement)
	if input.Action == "" {
		return toolError("[error] knowledge requires `action`."), nil
	}
	ref := knowlega.ScopeRef{OrgID: strings.TrimPrefix(t.orgScopeID, "org:"), ExternalScopeID: t.scopeLabel, Kind: "project", Name: t.scopeLabel}
	var value any
	var details any
	var err error
	switch input.Action {
	case "status":
		var status knowlega.ScopeStatus
		status, err = t.knowledge.Status(ref)
		if err == nil {
			value = knowledgeStatusValue(status)
			details = knowledgeDetails("status", "", "", "", "", nil)
		}
	case "search":
		if strings.TrimSpace(input.Query) == "" {
			return toolError("[error] knowledge search requires `query`."), nil
		}
		requestedLimit := boundedLimit(input.Limit, 10, 50)
		searchQuery := input.Query
		var candidateNames []string
		if input.Candidate != "" {
			// Candidate identity improves recall; a missing entity page must not prevent retrieval.
			names := []string{input.Candidate}
			if document, readErr := t.knowledge.Read(ref, input.Candidate); readErr == nil {
				names = mergeKnowledgeAliases(names, append([]string{document.Title}, document.Aliases...))
			}
			candidateNames = names
		}
		var results []knowledgecore.KnowledgeSearchResult
		if scoped, ok := t.knowledge.(interface {
			SearchScoped(context.Context, knowlega.ScopeRef, string, int, string) ([]knowledgecore.KnowledgeSearchResult, error)
		}); ok {
			results, err = scoped.SearchScoped(ctx, ref, searchQuery, requestedLimit, input.SearchScope)
		} else if input.SearchScope == "" || input.SearchScope == "all" {
			results, err = t.knowledge.Search(ctx, ref, searchQuery, requestedLimit)
		} else {
			return toolError("knowledge provider does not support scoped search"), nil
		}

		if len(candidateNames) > 0 {
			matchesCandidate := func(result knowledgecore.KnowledgeSearchResult) bool {
				text := strings.ToLower(result.Title + " " + result.Snippet + " " + result.Path)
				for _, name := range candidateNames {
					if strings.Contains(text, strings.ToLower(name)) {
						return true
					}
				}
				return false
			}
			sort.SliceStable(results, func(i, j int) bool { return matchesCandidate(results[i]) && !matchesCandidate(results[j]) })
		}
		value = results
		if err == nil {
			var status knowlega.ScopeStatus
			status, err = t.knowledge.Status(ref)
			if err == nil {
				state := knowledgeScopeState(status)
				detail := knowledgeDetails("search", input.Query, "", input.Candidate, input.Requirement, searchSources(results, false))
				detail["workspace_status"] = state
				detail["scope"] = input.SearchScope
				if input.SearchScope == "" {
					detail["scope"] = "all"
				}
				details = detail
				if len(results) == 0 {
					switch state {
					case knowledgeservice.KnowledgeWorkspaceQueued, knowledgeservice.KnowledgeWorkspaceProcessing:
						value = map[string]any{
							"status":    "pending",
							"message":   "project knowledge is still being processed; raw sources remain searchable while derived pages are incomplete",
							"workspace": knowledgeStatusValue(status),
							"results":   results,
						}
					case knowledgeservice.KnowledgeWorkspaceEmpty:
						return knowledgeWorkspaceError("knowledge_empty", "this project knowledge scope has no sources", status), nil
					case knowledgeservice.KnowledgeWorkspaceFailed:
						message := "project knowledge processing failed"
						if strings.TrimSpace(status.LastError) != "" {
							message += ": " + status.LastError
						}
						return knowledgeWorkspaceError("knowledge_failed", message, status), nil
					}
				}
			}
		}
	case "discover":
		if len(input.Requirements) == 0 {
			return toolError("[error] knowledge discover requires `requirements`."), nil
		}
		var candidates []knowledgecore.KnowledgeCandidate
		candidates, err = t.knowledge.Discover(ctx, ref, input.Requirements, boundedLimit(input.Limit, 12, 50))
		value = candidates
		if err == nil {
			details = knowledgeDetails("discover", "", "", "", "", candidateSources(candidates))
		}
	case "read":
		if strings.TrimSpace(input.Path) == "" {
			return toolError("[error] knowledge read requires `path`."), nil
		}
		var document knowledgeservice.KnowledgeDocument
		document, err = t.knowledge.Read(ref, input.Path)
		if err == nil {
			value = compactKnowledgeDocument(document)
			details = knowledgeDetails("read", "", document.Path, "", "", []map[string]any{knowledgeDocumentSource(document, !knowledgeservice.IsAggregateKnowledgePath(document.Path))})
		}
	case "list":
		var pages []knowledgeservice.KnowledgePage
		pages, err = t.knowledge.List(ref, input.Query, boundedLimit(input.Limit, 20, 50))
		value = pages
		if err == nil {
			details = knowledgeDetails("list", input.Query, "", "", "", pageSources(pages))
		}
	case "follow_links":
		if strings.TrimSpace(input.Path) == "" {
			return toolError("[error] knowledge follow_links requires `path`."), nil
		}
		var result knowledgeservice.KnowledgeFollowResult
		result, err = t.knowledge.FollowLinks(ref, input.Path, boundedLimit(input.Limit, 5, 25))
		value = result
		if err == nil {
			result.Documents = compactKnowledgeDocuments(result.Documents)
			value = result
			details = knowledgeDetails("follow_links", "", input.Path, "", "", documentSources(result.Documents, true))
		}
	case "graph":
		var documents []knowledgeservice.KnowledgeDocument
		documents, err = t.knowledge.Graph(ctx, ref, input.Query, input.SeedPaths, boundedLimit(input.Limit, 5, 25))
		value = documents
		if err == nil {
			documents = compactKnowledgeDocuments(documents)
			value = documents
			details = knowledgeDetails("graph", input.Query, "", "", "", documentSources(documents, true))
		}
	case "submit":
		submission := knowledgecore.KnowledgeSubmission{Question: input.Question, Answer: input.Answer, Status: input.Status, Candidate: input.Candidate, Requirements: input.Requirements, Checks: input.Checks, EvidencePaths: input.Evidence}
		if submission.Status == "" {
			submission.Status = "complete"
		}
		ledger, ledgerErr := t.knowledgeLedger(ctx)
		if ledgerErr != nil {
			return ToolResult{}, ledgerErr
		}
		submission = validateKnowledgeSubmission(submission, ledger)
		value = submission
		submitDetails := knowledgeDetails("submit", "", "", submission.Candidate, "", citationsAsSources(submission.Citations))
		submitDetails["status"] = submission.Status
		if len(submission.UnresolvedRequirementIDs) > 0 {
			submitDetails["unresolved_requirement_ids"] = submission.UnresolvedRequirementIDs
		}
		if len(submission.ValidationIssues) > 0 {
			submitDetails["validation_issues"] = submission.ValidationIssues
		}
		details = submitDetails
	case "writeback":
		if t.readOnly {
			return toolError("[this is a read-only wake — knowledge cannot be written back here]"), nil
		}
		if input.Submission == nil {
			return toolError("[error] knowledge writeback requires `submission` returned by submit."), nil
		}
		ledger, ledgerErr := t.knowledgeLedger(ctx)
		if ledgerErr != nil {
			return ToolResult{}, ledgerErr
		}
		validated := validateKnowledgeSubmission(*input.Submission, ledger)
		if validated.Status != "complete" {
			value = validated
			details = knowledgeDetails("writeback", "", "", validated.Candidate, "", citationsAsSources(validated.Citations))
			break
		}
		var result knowledgeservice.KnowledgeWritebackResult
		result, err = t.knowledge.Writeback(ctx, ref, input.Title, validated)
		value = result
		if err == nil {
			details = knowledgeDetails("writeback", "", result.Path, validated.Candidate, "", citationsAsSources(validated.Citations))
		}
	default:
		return toolError("[error] knowledge action must be status, search, discover, read, list, follow_links, graph, submit, or writeback."), nil
	}
	if err != nil {
		if input.Action == "status" {
			return knowledgeUnavailable(err.Error()), nil
		}
		return knowledgeActionFailed(input.Action, err.Error()), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ToolResult{}, err
	}
	detailBytes, _ := json.Marshal(details)
	return toolTextWithDetails(capToolText(string(encoded)), detailBytes), nil
}

type knowledgeLedger struct {
	Evidence         map[string]knowledgecore.KnowledgeCitation
	EvidenceSequence map[string]int
	Searches         []knowledgecore.KnowledgeActionRecord
}

func (t *coreToolContext) knowledgeLedger(ctx context.Context) (knowledgeLedger, error) {
	ledger := knowledgeLedger{
		Evidence:         map[string]knowledgecore.KnowledgeCitation{},
		EvidenceSequence: map[string]int{},
	}
	if t.sessions == nil {
		return ledger, nil
	}
	turnStartSeq := t.turnStartSeq
	turnStartFound := t.turnStartKnown && turnStartSeq >= 0
	if !t.turnStartKnown {
		latest, err := t.sessions.Entries(ctx, t.sessionID, 0, 0)
		if err != nil {
			return ledger, err
		}
		turnStartSeq, turnStartFound = latestUserEntrySequence(latest)
	}
	if !turnStartFound {
		// Without a user boundary there is no safe way to attribute prior
		// evidence to this turn. Treat the ledger as empty instead of allowing
		// historical tool results to satisfy a new submission.
		return ledger, nil
	}
	entries, err := t.sessions.Entries(ctx, t.sessionID, 0, turnStartSeq)
	if err != nil {
		return ledger, err
	}
	for _, entry := range entries {
		if entry.Type != "tool_result" {
			continue
		}
		var stored struct {
			Tool    string          `json:"tool"`
			Details json.RawMessage `json:"details"`
		}
		if json.Unmarshal(entry.Payload, &stored) != nil || stored.Tool != "knowledge" || len(stored.Details) == 0 {
			continue
		}
		var details struct {
			Kind            string `json:"kind"`
			Action          string `json:"action"`
			Query           string `json:"query"`
			Path            string `json:"path"`
			Candidate       string `json:"candidate"`
			RequirementID   string `json:"requirement_id"`
			WorkspaceStatus string `json:"workspace_status"`
			Sources         []struct {
				Path     string   `json:"path"`
				Title    string   `json:"title"`
				Kind     string   `json:"kind"`
				Aliases  []string `json:"aliases"`
				Evidence bool     `json:"evidence"`
			} `json:"sources"`
		}
		if json.Unmarshal(stored.Details, &details) != nil || details.Kind != "knowledge" {
			continue
		}
		if details.Action == "search" {
			resultPaths := make([]string, 0, len(details.Sources))
			for _, source := range details.Sources {
				if path := normalizeKnowledgeLedgerPath(source.Path); path != "" {
					resultPaths = append(resultPaths, path)
				}
			}
			ledger.Searches = append(ledger.Searches, knowledgecore.KnowledgeActionRecord{
				Action: "search", Query: details.Query, Candidate: details.Candidate, RequirementID: details.RequirementID,
				WorkspaceStatus: details.WorkspaceStatus, ResultCount: len(details.Sources), Sequence: entry.Sequence, ResultPaths: resultPaths,
			})
		}
		for _, source := range details.Sources {
			if source.Evidence && strings.TrimSpace(source.Path) != "" && !knowledgeservice.IsAggregateKnowledgePath(source.Path) {
				path := normalizeKnowledgeLedgerPath(source.Path)
				citation := ledger.Evidence[path]
				citation.Path = source.Path
				citation.Title = source.Title
				citation.Kind = source.Kind
				citation.Aliases = mergeKnowledgeAliases(citation.Aliases, source.Aliases)
				ledger.Evidence[path] = citation
				ledger.EvidenceSequence[path] = max(ledger.EvidenceSequence[path], entry.Sequence)
			}
		}
	}
	return ledger, nil
}

func latestUserEntrySequence(entries []data.SessionEntry) (int, bool) {
	latest := -1
	for _, entry := range entries {
		if entry.Type == "user" && entry.Sequence > latest {
			latest = entry.Sequence
		}
	}
	return latest, latest >= 0
}

func validateKnowledgeSubmission(submission knowledgecore.KnowledgeSubmission, ledger knowledgeLedger) knowledgecore.KnowledgeSubmission {
	submission.Question = strings.TrimSpace(submission.Question)
	submission.Answer = strings.TrimSpace(submission.Answer)
	submission.Candidate = strings.TrimSpace(submission.Candidate)
	submission.ValidationIssues = nil
	addIssue := func(code, requirementID, message string) {
		for _, issue := range submission.ValidationIssues {
			if issue.Code == code && issue.RequirementID == requirementID {
				return
			}
		}
		submission.ValidationIssues = append(submission.ValidationIssues, knowledgecore.KnowledgeValidationIssue{
			Code: code, RequirementID: requirementID, Message: message,
		})
	}
	requirements := map[string]knowledgecore.KnowledgeRequirement{}
	orderedIDs := make([]string, 0, len(submission.Requirements))
	invalidRequirementIDs := map[string]bool{}
	malformed := false
	if len(submission.Requirements) == 0 {
		malformed = true
		addIssue("missing_requirements", "", "submit requires the full non-empty requirements array")
	}
	for _, requirement := range submission.Requirements {
		requirement.ID = strings.TrimSpace(requirement.ID)
		requirement.Text = strings.TrimSpace(requirement.Text)
		requirement.Kind = strings.ToLower(strings.TrimSpace(requirement.Kind))
		if requirement.Kind == "" {
			requirement.Kind = "positive"
		}
		if requirement.ID == "" || requirement.Text == "" || (requirement.Kind != "positive" && requirement.Kind != "negative") {
			malformed = true
			invalidRequirementIDs[requirement.ID] = true
			addIssue("invalid_requirement", requirement.ID, "each requirement needs a non-empty id and text, and kind must be positive or negative")
			continue
		}
		if _, exists := requirements[requirement.ID]; exists {
			malformed = true
			invalidRequirementIDs[requirement.ID] = true
			addIssue("duplicate_requirement", requirement.ID, "requirement ids must be unique")
			continue
		}
		requirements[requirement.ID] = requirement
		orderedIDs = append(orderedIDs, requirement.ID)
	}
	checks := map[string]knowledgecore.KnowledgeEvidenceCheck{}
	invalidCheckIDs := map[string]bool{}
	if len(submission.Checks) == 0 {
		malformed = true
		addIssue("missing_checks", "", "submit requires exactly one check per requirement")
	}
	for _, check := range submission.Checks {
		check.RequirementID = strings.TrimSpace(check.RequirementID)
		check.Status = strings.ToLower(strings.TrimSpace(check.Status))
		if check.RequirementID == "" || !knowledgeEvidenceCheckStatus(check.Status) {
			malformed = true
			addIssue("invalid_check", check.RequirementID, "each check needs a requirement_id and a valid status")
		}
		if _, known := requirements[check.RequirementID]; !known {
			invalidCheckIDs[check.RequirementID] = true
			addIssue("unknown_requirement_check", check.RequirementID, "check requirement_id must reference a submitted requirement")
			continue
		}
		if _, exists := checks[check.RequirementID]; exists {
			malformed = true
			invalidCheckIDs[check.RequirementID] = true
			addIssue("duplicate_check", check.RequirementID, "provide exactly one check per requirement")
			continue
		}
		checks[check.RequirementID] = check
	}
	unresolved := map[string]bool{}
	for id := range invalidRequirementIDs {
		if id != "" {
			unresolved[id] = true
		}
	}
	for id := range invalidCheckIDs {
		if id != "" {
			unresolved[id] = true
		}
	}
	if submission.Candidate == "" {
		for _, id := range orderedIDs {
			unresolved[id] = true
		}
		malformed = true
		addIssue("missing_candidate", "", "submit requires a non-empty candidate")
	}

	used := map[string]bool{}
	for _, requested := range submission.EvidencePaths {
		path := normalizeKnowledgeLedgerPath(requested)
		if _, ok := ledger.Evidence[path]; ok {
			used[path] = true
		}
	}
	for _, id := range orderedIDs {
		requirement := requirements[id]
		check, ok := checks[id]
		if !ok {
			unresolved[id] = true
			addIssue("missing_check", id, "provide exactly one evidence check for this requirement")
			continue
		}
		allowedStatus := check.Status == "supported"
		if requirement.Kind == "negative" && check.Status != "supported" {
			if check.Status != "not_found_in_corpus" {
				addIssue("negative_check_requires_not_found", id, "negative checks need supported with explicit negative evidence, or not_found_in_corpus with a current-turn relevant zero-result search; omission from selected citations is not evidence")
			} else {
				allowedStatus = knowledgeNegativeSearchRecorded(ledger.Searches, submission.Candidate, id, ledger.Evidence)
				if !allowedStatus {
					if knowledgeNegativeSearchFoundResults(ledger.Searches, submission.Candidate, id, ledger.Evidence) {
						addIssue("negative_search_found_results", id, "a current-turn candidate-specific search returned results; read the hit and refine the search to the positive event terms from the requirement before claiming not_found_in_corpus")
					} else {
						addIssue("negative_search_not_recorded", id, "not_found_in_corpus requires a current-turn zero-result search for the positive event terms, carrying the same candidate and requirement_id while the workspace is ready")
					}
				}
			}
			if !allowedStatus {
				for index := range submission.ValidationIssues {
					issue := &submission.ValidationIssues[index]
					if issue.RequirementID == id && (issue.Code == "negative_check_requires_not_found" || issue.Code == "negative_search_not_recorded") {
						issue.Repair = &knowledgecore.KnowledgeRepairAction{
							Action: "search", Query: knowledgeNegativeConditionProbe(requirement.Text), Candidate: firstKnowledgeCandidateName(submission.Candidate), RequirementID: id,
						}
						break
					}
				}
			}
		}
		if !allowedStatus {
			unresolved[id] = true
			if requirement.Kind != "negative" {
				addIssue("check_not_supported", id, "the check status does not resolve this requirement")
			}
		}
		validPath := false
		for _, path := range check.EvidencePaths {
			normalized := normalizeKnowledgeLedgerPath(path)
			if _, ok := ledger.Evidence[normalized]; ok {
				validPath = true
				used[normalized] = true
			} else {
				unresolved[id] = true
				addIssue("evidence_not_in_current_turn", id, "every cited check path must be current-turn read, follow_links, or graph evidence")
			}
		}
		if check.Status == "supported" && !validPath {
			unresolved[id] = true
			addIssue("evidence_not_in_current_turn", id, "supported checks must cite at least one current-turn read, follow_links, or graph evidence path")
		}

	}
	if submission.Question == "" {
		for _, id := range orderedIDs {
			unresolved[id] = true
		}
		addIssue("missing_question", "", "submit requires a non-empty question")
	}
	if submission.Answer == "" {
		for _, id := range orderedIDs {
			unresolved[id] = true
		}
		addIssue("missing_answer", "", "submit requires a non-empty answer")
	}
	if len(used) == 0 && !(len(requirements) > 0 && len(ledger.Evidence) == 0 && knowledgeRecallReturnedNoCandidates(ledger.Searches)) {
		for _, id := range orderedIDs {
			unresolved[id] = true
		}
		addIssue("no_evidence_citations", "", "submit must cite evidence collected in the current turn")
	}
	submission.Citations = nil
	for path := range used {
		submission.Citations = append(submission.Citations, ledger.Evidence[path])
	}
	sort.Slice(submission.Citations, func(i, j int) bool { return submission.Citations[i].Path < submission.Citations[j].Path })
	submission.UnresolvedRequirementIDs = nil
	for id := range unresolved {
		if _, known := requirements[id]; !known && id != "" {
			submission.UnresolvedRequirementIDs = append(submission.UnresolvedRequirementIDs, id)
		}
	}
	for _, id := range orderedIDs {
		if unresolved[id] {
			submission.UnresolvedRequirementIDs = append(submission.UnresolvedRequirementIDs, id)
		}
	}
	sort.Strings(submission.UnresolvedRequirementIDs)
	if malformed || len(submission.UnresolvedRequirementIDs) > 0 || len(submission.Citations) == 0 {
		submission.Status = "incomplete"
	} else {
		submission.Status = "complete"
	}
	return submission
}

func knowledgeEvidenceCheckStatus(status string) bool {
	switch status {
	case "supported", "contradicted", "unknown", "not_found_in_corpus":
		return true
	default:
		return false
	}
}

func knowledgeNegativeSearchRecorded(records []knowledgecore.KnowledgeActionRecord, candidate, requirementID string, evidence map[string]knowledgecore.KnowledgeCitation) bool {
	candidateNames := knowledgeCandidateSearchNames(candidate, evidence)
	requirementID = strings.TrimSpace(requirementID)
	for _, record := range records {
		if record.Action != "search" || strings.TrimSpace(record.RequirementID) != requirementID || len(candidateNames) == 0 || record.ResultCount != 0 || record.WorkspaceStatus != knowledgeservice.KnowledgeWorkspaceReady || !knowledgeSearchCandidateMatches(record.Candidate, candidateNames) || knowledgeCandidateConditionQuery(record.Query, candidateNames) == "" {
			continue
		}
		return true
	}
	return false
}

func knowledgeNegativeSearchFoundResults(records []knowledgecore.KnowledgeActionRecord, candidate, requirementID string, evidence map[string]knowledgecore.KnowledgeCitation) bool {
	candidateNames := knowledgeCandidateSearchNames(candidate, evidence)
	requirementID = strings.TrimSpace(requirementID)
	for _, record := range records {
		if record.Action != "search" || strings.TrimSpace(record.RequirementID) != requirementID || record.ResultCount <= 0 || record.WorkspaceStatus != knowledgeservice.KnowledgeWorkspaceReady || !knowledgeSearchCandidateMatches(record.Candidate, candidateNames) || knowledgeCandidateConditionQuery(record.Query, candidateNames) == "" {
			continue
		}
		return true
	}
	return false
}

func knowledgeSearchCandidateMatches(candidate string, candidateNames []string) bool {
	for _, candidateName := range candidateNames {
		if strings.EqualFold(strings.TrimSpace(candidate), candidateName) {
			return true
		}
	}
	return false
}

func knowledgeNegativeConditionProbe(requirementText string) string {
	condition := strings.TrimSpace(requirementText)
	lower := strings.ToLower(condition)
	for _, prefix := range []string{"从来没有", "从未", "不曾", "未曾", "没有", "并未", "未", "不", "has never ", "have never ", "did not ", "does not ", "has not ", "have not ", "never ", "not "} {
		if strings.HasPrefix(lower, prefix) {
			condition = strings.TrimSpace(condition[len(prefix):])
			break
		}
	}
	if condition == "" {
		return strings.TrimSpace(requirementText)
	}
	return condition
}

func knowledgeCandidateConditionQuery(query string, candidateNames []string) string {
	condition := strings.TrimSpace(query)
	names := append([]string(nil), candidateNames...)
	sort.SliceStable(names, func(i, j int) bool { return len([]rune(names[i])) > len([]rune(names[j])) })
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		pattern, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(name))
		if err == nil {
			condition = pattern.ReplaceAllString(condition, " ")
		}
	}
	condition = strings.TrimFunc(condition, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	return strings.Join(strings.Fields(condition), " ")
}

func knowledgeCandidateConditionFields(query string) []string {
	var fields []string
	for _, field := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	}) {
		field = strings.TrimSpace(field)
		if field != "" && !containsStringFold(fields, field) {
			fields = append(fields, field)
		}
	}
	return fields
}

func knowledgeCandidateSearchNames(candidate string, evidence map[string]knowledgecore.KnowledgeCitation) []string {
	names := knowledgeCandidateIdentityNames(candidate)
	for path, citation := range evidence {
		if !strings.HasPrefix(normalizeKnowledgeLedgerPath(path), "wiki/") || knowledgeservice.IsAggregateKnowledgePath(path) {
			continue
		}
		matches := false
		for _, name := range names {
			if strings.EqualFold(strings.TrimSpace(citation.Title), name) || containsStringFold(citation.Aliases, name) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		names = mergeKnowledgeAliases(names, append([]string{citation.Title}, citation.Aliases...))
	}
	return names
}

func knowledgeCandidateIdentityNames(candidate string) []string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return nil
	}
	names := []string{candidate}
	for _, opener := range []string{"（", "(", "【", "["} {
		if index := strings.Index(candidate, opener); index > 0 {
			primary := strings.TrimSpace(candidate[:index])
			if primary != "" && !containsStringFold(names, primary) {
				names = append(names, primary)
			}
		}
	}
	return names
}

func firstKnowledgeCandidateName(candidate string) string {
	names := knowledgeCandidateIdentityNames(candidate)
	if len(names) == 0 {
		return strings.TrimSpace(candidate)
	}
	if len(names) > 1 {
		return names[1]
	}
	return names[0]
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func knowledgeRecallReturnedNoCandidates(records []knowledgecore.KnowledgeActionRecord) bool {
	for _, record := range records {
		if record.WorkspaceStatus != knowledgeservice.KnowledgeWorkspaceReady {
			continue
		}
		if record.Action == "search" && record.ResultCount > 0 {
			return false
		}
	}
	for _, record := range records {
		if record.Action == "search" && record.WorkspaceStatus == knowledgeservice.KnowledgeWorkspaceReady {
			return true
		}
	}
	return false
}

func knowledgeWorkspaceStillPending(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case knowledgeservice.KnowledgeWorkspaceEmpty, knowledgeservice.KnowledgeWorkspaceQueued, knowledgeservice.KnowledgeWorkspaceProcessing, knowledgeservice.KnowledgeWorkspaceFailed:
		return true
	case knowledgeservice.KnowledgeWorkspaceReady:
		return false
	default:
		return true
	}
}

func normalizeKnowledgeLedgerPath(path string) string {
	return strings.ToLower(filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(path), "./")))
}

func knowledgeDetails(action, query, path, candidate, requirementID string, sources []map[string]any) map[string]any {
	return map[string]any{"kind": "knowledge", "action": action, "query": query, "path": path, "candidate": candidate, "requirement_id": requirementID, "sources": sources}
}

func knowledgeScopeState(status knowlega.ScopeStatus) string {
	if strings.TrimSpace(status.State) != "" {
		return status.State
	}
	if status.Ready {
		return knowledgeservice.KnowledgeWorkspaceReady
	}
	return knowledgeservice.KnowledgeWorkspaceEmpty
}

func knowledgeStatusValue(status knowlega.ScopeStatus) map[string]any {
	value := map[string]any{
		"scope":         map[string]string{"externalScopeId": status.Scope.ExternalScopeID, "kind": status.Scope.Kind},
		"projectId":     status.ProjectID,
		"projectName":   status.Scope.Name,
		"status":        knowledgeScopeState(status),
		"wikiPageCount": status.WikiPageCount,
		"sourceCount":   status.SourceCount,
		"queue": map[string]int{
			"pending": status.Queue.Pending, "processing": status.Queue.Processing, "done": status.Queue.Done, "failed": status.Queue.Failed, "total": status.Queue.Total,
		},
		"ready": status.Ready,
	}
	if strings.TrimSpace(status.LastError) != "" {
		value["lastError"] = status.LastError
	}
	if status.LastSuccessfulAt != nil {
		value["lastSuccessfulAt"] = status.LastSuccessfulAt.UTC().Format(time.RFC3339Nano)
	}
	return value
}

func knowledgeSource(path, title, kind string, evidence bool) map[string]any {
	return map[string]any{"path": path, "title": title, "kind": kind, "evidence": evidence}
}

func knowledgeSourceWithAliases(path, title, kind string, aliases []string, evidence bool) map[string]any {
	source := knowledgeSource(path, title, kind, evidence)
	if aliases = mergeKnowledgeAliases(nil, aliases); len(aliases) > 0 {
		source["aliases"] = aliases
	}
	return source
}

func compactKnowledgeDocument(document knowledgeservice.KnowledgeDocument) knowledgeservice.KnowledgeDocument {
	document.Content = compactKnowledgeContent(document.Content, knowledgeDocumentContentLimit)
	return document
}

func compactKnowledgeDocuments(documents []knowledgeservice.KnowledgeDocument) []knowledgeservice.KnowledgeDocument {
	if len(documents) == 0 {
		return documents
	}
	compacted := make([]knowledgeservice.KnowledgeDocument, len(documents))
	for index, document := range documents {
		compacted[index] = compactKnowledgeDocument(document)
	}
	return compacted
}

func compactKnowledgeContent(content string, limit int) string {
	if limit <= 0 || len([]rune(content)) <= limit {
		return content
	}
	runes := []rune(content)
	head := limit * 2 / 3
	tail := limit - head
	return string(runes[:head]) + "\n\n[… Knowledge 内容已截断；请用更具体的 search/read 定位剩余证据 …]\n\n" + string(runes[len(runes)-tail:])
}

func knowledgeDocumentSource(document knowledgeservice.KnowledgeDocument, evidence bool) map[string]any {
	return knowledgeSourceWithAliases(document.Path, document.Title, document.Kind, document.Aliases, evidence)
}

func searchSources(results []knowledgecore.KnowledgeSearchResult, evidence bool) []map[string]any {
	sources := make([]map[string]any, 0, len(results))
	for _, result := range results {
		sources = append(sources, knowledgeSource(result.Path, result.Title, result.Kind, evidence))
	}
	return sources
}

func candidateSources(results []knowledgecore.KnowledgeCandidate) []map[string]any {
	sources := make([]map[string]any, 0, len(results))
	for _, result := range results {
		sources = append(sources, knowledgeSource(result.Path, result.Title, result.Kind, false))
	}
	return sources
}

func pageSources(results []knowledgeservice.KnowledgePage) []map[string]any {
	sources := make([]map[string]any, 0, len(results))
	for _, result := range results {
		sources = append(sources, knowledgeSource(result.Path, result.Title, result.Type, false))
	}
	return sources
}

func documentSources(results []knowledgeservice.KnowledgeDocument, evidence bool) []map[string]any {
	sources := make([]map[string]any, 0, len(results))
	for _, result := range results {
		sources = append(sources, knowledgeDocumentSource(result, evidence && !knowledgeservice.IsAggregateKnowledgePath(result.Path)))
	}
	return sources
}

func citationsAsSources(citations []knowledgecore.KnowledgeCitation) []map[string]any {
	sources := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		sources = append(sources, knowledgeSourceWithAliases(citation.Path, citation.Title, citation.Kind, citation.Aliases, true))
	}
	return sources
}

func mergeKnowledgeAliases(existing, incoming []string) []string {
	merged := append([]string(nil), existing...)
	for _, alias := range incoming {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		found := false
		for _, current := range merged {
			if strings.EqualFold(strings.TrimSpace(current), alias) {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, alias)
		}
	}
	return merged
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func (t *coreToolContext) workspacePath(relative string, writing bool) (string, error) {
	return secureWorkspacePath(t.workspaceRoot, t.workspaceKey, relative, writing)
}

func secureWorkspacePath(workspaceRoot, workspaceKey, relative string, writing bool) (string, error) {
	relative = filepath.FromSlash(strings.TrimSpace(relative))
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("path must be relative to the workspace")
	}
	root := filepath.Join(workspaceRoot, safeWorkspaceKey(workspaceKey))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(root, relative))
	if target == root || !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", errors.New("path escapes the workspace")
	}
	check := target
	if writing {
		check = filepath.Dir(target)
	}
	for {
		resolved, err := filepath.EvalSymlinks(check)
		if err == nil {
			if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
				return "", errors.New("path escapes the workspace through a symlink")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) || check == root {
			break
		}
		check = filepath.Dir(check)
	}
	return target, nil
}

func coreToolDefinitions(knowledge bool) []ToolDefinition {
	definitions := []ToolDefinition{
		toolDefinition("execute", "Run a shell command in the isolated sandbox and return its stdout/stderr/exit code.", `{"type":"object","properties":{"command":{"type":"string"},"purpose":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1}},"required":["command","purpose"]}`),
		toolDefinition("read", "Read a file from the workspace (scope, then global). Returns its contents.", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		toolDefinition("write", "Write a file to the writable workspace scope and optionally share it.", `{"type":"object","properties":{"path":{"type":"string"},"data":{"type":"string"},"share":{"type":"array","items":{"type":"object","properties":{"scope":{"type":"string"},"permission":{"enum":["read","write"]}},"required":["scope"]}}},"required":["path"]}`),
		toolDefinition("publish", "Publish a workspace directory as a durable internal web app.", `{"type":"object","properties":{"dir":{"type":"string"},"entrypoint":{"type":"string"},"name":{"type":"string"},"renameFrom":{"type":"string"},"env":{"type":"object","additionalProperties":{"type":"string"}},"rollbackTo":{"type":"integer"},"share":{"type":"array"}}}`),
		toolDefinition("memory", "Read or change this scope's durable memory notebook.", `{"type":"object","properties":{"action":{"enum":["search","remember","read","rewrite"]},"query":{"type":"string"},"limit":{"type":"integer"},"facts":{"type":"array","items":{"type":"string"}},"content":{"type":"string"}},"required":["action"]}`),
		toolDefinition("history", "Search this conversation's durable transcript.", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
	}
	if knowledge {
		definitions = append(definitions,
			toolDefinition("knowledge", "Use deterministic project knowledge facts. Status distinguishes empty/queued/processing/ready/failed; a pending search is not proof of absence. Search/discover/list are navigation only; read/follow_links/graph return evidence. For multi-constraint identity questions, discover with the full requirements ranks common-subject entity candidates and should be preferred over repeated search paraphrases; preserve subject/object roles. Choose evidence actions freely; a current-turn read or graph result may support multiple requirements without a preceding search. A dedicated candidate page is not required. Every submit call requires non-empty question, answer, candidate, the full requirements array, and exactly one check per requirement. Candidate and requirement_id are optional search metadata; keep candidate only as filtering metadata and use only the requirement's event terms in query. For a negative requirement, search the positive event that would disprove it, for example 到过 花果山. Explicit negative evidence may resolve a supported check. Otherwise use not_found_in_corpus only after a current-turn relevant zero-result candidate-specific search in a ready workspace; omission in citations and pending searches cannot prove absence. A matching hit conflicts with absence and must be read or refined before submission. Submit validates only the current turn's evidence ledger and returns validation_issues when incomplete; writeback explicitly saves a validated submission.", `{"type":"object","properties":{"action":{"enum":["status","search","discover","read","list","follow_links","graph","submit","writeback"]},"query":{"type":"string","description":"For candidate-specific searches, use only the matching requirement event terms and do not repeat candidate. For a negative requirement, use the positive disproof event terms and do not add unrelated terms."},"scope":{"enum":["all","wiki","raw"],"description":"Search scope; defaults to all. Use raw for original source evidence."},"path":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"seed_paths":{"type":"array","items":{"type":"string"}},"requirements":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"},"kind":{"enum":["positive","negative"]}},"required":["id","text"]}},"question":{"type":"string","description":"Required and non-empty for action=submit."},"answer":{"type":"string","description":"Required and non-empty for action=submit."},"candidate":{"type":"string","description":"Required for action=submit; identify the candidate supported by current-turn evidence; a dedicated wiki page is not required. For candidate-specific searches, pass it only as filtering metadata."},"checks":{"type":"array","description":"Required for action=submit; provide exactly one check for every requirement. Supported checks must cite current-turn read, follow_links, or graph evidence. Negative supported checks need explicit negative evidence; omission only permits not_found_in_corpus after a relevant search.","items":{"type":"object","properties":{"requirement_id":{"type":"string"},"status":{"enum":["supported","contradicted","unknown","not_found_in_corpus"]},"evidence_paths":{"type":"array","items":{"type":"string"}},"explanation":{"type":"string"}},"required":["requirement_id","status"]}},"evidence_paths":{"type":"array","items":{"type":"string"}},"status":{"enum":["complete","incomplete"]},"requirement_id":{"type":"string","description":"Optional association with a submitted requirement; include for absence checks."},"title":{"type":"string"},"submission":{"type":"object"}},"required":["action"]}`),
		)
	}
	definitions = append(definitions, toolDefinition("background", "Run and manage long commands on the durable workspace computer.", `{"type":"object","properties":{"action":{"enum":["start","poll","write","stop","list","watch","unwatch"]},"command":{"type":"string"},"process_id":{"type":"string"},"data":{"type":"string"},"since_cursor":{"type":"integer"},"pattern":{"type":"string"},"instructions":{"type":"string"},"monitor_id":{"type":"string"},"wait_seconds":{"type":"integer"},"max_bytes":{"type":"integer"},"signal":{"enum":["TERM","KILL","INT","HUP","QUIT"]},"timeout_seconds":{"type":"integer"}},"required":["action"]}`))
	return definitions
}

func toolDefinition(name, description, schema string) ToolDefinition {
	mode := "sequential"
	if name == "read" || name == "history" {
		mode = "parallel"
	}
	return ToolDefinition{Name: name, Description: description, InputSchema: json.RawMessage(schema), ExecutionMode: mode}
}

func toolText(value string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: "text", Text: value}}, Details: json.RawMessage(`{}`)}
}

func toolTextWithDetails(value string, details json.RawMessage) ToolResult {
	if len(details) == 0 || string(details) == "null" {
		details = json.RawMessage(`{}`)
	}
	return ToolResult{Content: []ToolContent{{Type: "text", Text: value}}, Details: details}
}

func toolError(value string) ToolResult {
	result := toolText(value)
	result.IsError = true
	return result
}

func knowledgeUnavailable(message string) ToolResult {
	encoded, _ := json.Marshal(map[string]any{"ok": false, "code": "knowledge_unavailable", "message": message})
	return toolError(string(encoded))
}

func knowledgeActionFailed(action, message string) ToolResult {
	encoded, _ := json.Marshal(map[string]any{"ok": false, "code": "knowledge_action_failed", "action": action, "message": message})
	return toolError(string(encoded))
}

func knowledgeWorkspaceError(code, message string, status knowlega.ScopeStatus) ToolResult {
	encoded, _ := json.Marshal(map[string]any{
		"ok": false, "code": code, "message": message, "workspace": knowledgeStatusValue(status),
	})
	return toolError(string(encoded))
}

func decodeToolArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	return json.Unmarshal(raw, target)
}

func capToolText(value string) string {
	if len(value) <= toolResultLimit {
		return value
	}
	return value[:toolResultLimit] + "…[truncated]"
}

func boundedLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func safeWorkspaceKey(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	key := strings.Trim(builder.String(), ".")
	if key == "" {
		key = "workspace"
	}
	sum := sha256.Sum256([]byte(value))
	return key + "-" + fmt.Sprintf("%x", sum[:6])
}

func memoryQuery(body, query string, limit int) []string {
	terms := strings.Fields(strings.ToLower(query))
	result := []string{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "* ") {
			continue
		}
		text := strings.TrimSpace(trimmed[2:])
		lower := strings.ToLower(text)
		matches := true
		for _, term := range terms {
			if !strings.Contains(lower, term) {
				matches = false
				break
			}
		}
		if matches {
			result = append(result, text)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func foldMemoryFacts(existing string, facts []string, now time.Time) (string, int) {
	seen := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			seen[normalizeFact(trimmed[2:])] = true
		}
	}
	added := []string{}
	for _, fact := range facts {
		clean := strings.Join(strings.Fields(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(fact, "- "), "* "))), " ")
		key := normalizeFact(clean)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		added = append(added, fmt.Sprintf("- (%s) %s", now.Format("2006-01-02"), clean))
	}
	if len(added) == 0 {
		return existing, 0
	}
	body := strings.TrimRightFunc(existing, unicode.IsSpace)
	if body == "" {
		body = "# Memory\n\n" + strings.Join(added, "\n")
	} else {
		body += "\n" + strings.Join(added, "\n")
	}
	lines := strings.Split(body, "\n")
	bulletIndexes := []int{}
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			bulletIndexes = append(bulletIndexes, index)
		}
	}
	if overflow := len(bulletIndexes) - 300; overflow > 0 {
		drop := map[int]bool{}
		for _, index := range bulletIndexes[:overflow] {
			drop[index] = true
		}
		kept := lines[:0]
		for index, line := range lines {
			if !drop[index] {
				kept = append(kept, line)
			}
		}
		lines = kept
	}
	return strings.Join(lines, "\n") + "\n", len(added)
}

func normalizeFact(value string) string {
	fields := strings.Fields(value)
	if len(fields) > 0 && len(fields[0]) == 12 && fields[0][0] == '(' && fields[0][11] == ')' {
		fields = fields[1:]
	}
	return strings.ToLower(strings.Join(fields, " "))
}

func allBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func pluralSuffix(value int) string {
	if value == 1 {
		return ""
	}
	return "s"
}

func searchHistory(entries []data.SessionEntry, query string, limit int) []string {
	terms := strings.Fields(strings.ToLower(query))
	result := []string{}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence > entries[j].Sequence })
	for _, entry := range entries {
		var payload map[string]any
		_ = json.Unmarshal(entry.Payload, &payload)
		text, _ := payload["text"].(string)
		if strings.TrimSpace(text) == "" {
			text = string(entry.Payload)
		}
		name, _ := payload["name"].(string)
		haystack := strings.ToLower(strings.TrimSpace(name + " " + text))
		matches := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		clipped := []rune(strings.TrimSpace(text))
		if len(clipped) > 500 {
			clipped = append(clipped[:500], '…')
		}
		who := ":"
		if strings.TrimSpace(name) != "" && entry.Type == "user" {
			who = " " + strings.TrimSpace(name) + ":"
		}
		result = append(result, fmt.Sprintf("%s#%d (%s)%s %s", entry.Type, entry.Sequence, time.UnixMilli(entry.CreatedAt).UTC().Format(time.RFC3339Nano), who, string(clipped)))
		if len(result) == limit {
			break
		}
	}
	return result
}
