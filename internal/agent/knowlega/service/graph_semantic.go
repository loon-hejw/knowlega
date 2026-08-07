package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/codegraph"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/config"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmclient"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
)

type GraphSemanticEnricher interface {
	Enrich(context.Context, string) (int, error)
}

type GraphRelationAgent interface {
	InferRelations(context.Context, GraphRelationInput) ([]GraphRelationSuggestion, error)
}

type GraphRelationInput struct {
	Nodes []GraphRelationNode `json:"nodes"`
	Edges []GraphRelationEdge `json:"exact_edges"`
}

type GraphRelationNode struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	SourceRef string `json:"source_ref"`
}

type GraphRelationEdge struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"`
}

type GraphRelationSuggestion struct {
	Source          string  `json:"source"`
	Target          string  `json:"target"`
	Relation        string  `json:"relation"`
	ConfidenceScore float64 `json:"confidence_score"`
	Rationale       string  `json:"rationale"`
}

type graphSemanticEnricher struct {
	agent GraphRelationAgent
}

type llmGraphRelationAgent struct {
	client          llmclient.Client
	retry           llmretry.Options
	maxInputChars   int
	maxOutputTokens int
	disableThinking bool
	model           string
}

func NewLLMGraphSemanticEnricher(cfg config.LLMConfig) (GraphSemanticEnricher, error) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("graph semantic enrichment requires llm.api_key and llm.model")
	}
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	agent := &llmGraphRelationAgent{
		client: llmclient.Client{
			BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Model: cfg.Model, Protocol: cfg.Protocol,
			UserAgent: cfg.UserAgent, AnthropicVersion: cfg.AnthropicVersion, HTTPClient: &http.Client{Timeout: timeout},
		},
		retry:         llmretry.Options{Retries: cfg.Retries, BaseDelay: cfg.RetryBaseDelay.Duration, MaxDelay: cfg.RetryMaxDelay.Duration, MaxElapsed: cfg.OperationTimeout.Duration},
		maxInputChars: cfg.MaxInputChars, maxOutputTokens: minPositive(cfg.MaxOutputTokens, 4096),
		disableThinking: cfg.DisableThinking, model: cfg.Model,
	}
	return &graphSemanticEnricher{agent: agent}, nil
}

func (e *graphSemanticEnricher) Enrich(ctx context.Context, projectPath string) (int, error) {
	relationsArtifactMu.Lock()
	defer relationsArtifactMu.Unlock()
	graph, err := BuildUnifiedProjectGraph(projectPath)
	if err != nil {
		return 0, err
	}
	input := graphRelationInput(graph, 180, 260)
	if len(input.Nodes) < 2 {
		return 0, nil
	}
	suggestions, err := e.agent.InferRelations(ctx, input)
	if err != nil {
		return 0, err
	}
	nodes := map[string]GraphRelationNode{}
	for _, node := range input.Nodes {
		nodes[node.ID] = node
	}
	allowed := map[string]bool{
		string(codegraph.RelDocuments): true, string(codegraph.RelReferences): true,
		string(codegraph.RelCites): true, string(codegraph.RelSupports): true,
		string(codegraph.RelContradicts): true, string(codegraph.RelRelatedTo): true,
	}
	artifact := loadRelationsArtifact(projectPath)
	if artifact.Version == 0 {
		artifact.Version = 1
	}
	merged := map[string]WikiGraphAPIEdge{}
	for _, edge := range artifact.Edges {
		merged[edge.Source+"\x00"+edge.Target+"\x00"+edge.Relation] = edge
	}
	accepted := 0
	for _, suggestion := range suggestions {
		source, sourceOK := nodes[suggestion.Source]
		target, targetOK := nodes[suggestion.Target]
		relation := strings.ToUpper(strings.TrimSpace(suggestion.Relation))
		if !sourceOK || !targetOK || source.ID == target.ID || !allowed[relation] {
			continue
		}
		score := suggestion.ConfidenceScore
		if score > 0.95 {
			score = 0.95
		}
		if score < 0.1 {
			continue
		}
		confidence := "AMBIGUOUS"
		if score >= 0.55 {
			confidence = "INFERRED"
		} else if score > 0.3 {
			score = 0.3
		}
		edge := WikiGraphAPIEdge{
			ID:     core.StableID("llm-relation", source.ID, target.ID, relation),
			Source: source.ID, Target: target.ID, Kind: strings.ToLower(relation), Relation: relation,
			Confidence: confidence, ConfidenceScore: score, Weight: score,
			Evidence: []string{source.SourceRef, target.SourceRef},
			Props:    map[string]any{"generated_by": "llm", "rationale": strings.TrimSpace(suggestion.Rationale)},
		}
		key := edge.Source + "\x00" + edge.Target + "\x00" + edge.Relation
		if existing, ok := merged[key]; ok && existing.ConfidenceScore >= edge.ConfidenceScore {
			continue
		}
		merged[key] = edge
		accepted++
	}
	artifact.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	artifact.GeneratedBy = "knowledge-core+llm"
	artifact.Edges = artifact.Edges[:0]
	for _, edge := range merged {
		artifact.Edges = append(artifact.Edges, edge)
	}
	sort.Slice(artifact.Edges, func(i, j int) bool { return artifact.Edges[i].ID < artifact.Edges[j].ID })
	if err := saveRelationsArtifact(projectPath, artifact); err != nil {
		return 0, err
	}
	return accepted, nil
}

func (a *llmGraphRelationAgent) InferRelations(ctx context.Context, input GraphRelationInput) ([]GraphRelationSuggestion, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	for a.maxInputChars > 0 && len(data) > a.maxInputChars && len(input.Nodes) > 2 {
		input.Nodes = input.Nodes[:len(input.Nodes)*3/4]
		allowed := map[string]bool{}
		for _, node := range input.Nodes {
			allowed[node.ID] = true
		}
		filtered := input.Edges[:0]
		for _, edge := range input.Edges {
			if allowed[edge.Source] && allowed[edge.Target] {
				filtered = append(filtered, edge)
			}
		}
		input.Edges = filtered
		data, err = json.Marshal(input)
		if err != nil {
			return nil, err
		}
	}
	if a.maxInputChars > 0 && len(data) > a.maxInputChars {
		return nil, fmt.Errorf("graph semantic input exceeds llm.max_input_chars")
	}
	content, err := llmretry.Do(ctx, a.retry, nil, func(_ int) (string, bool, error) {
		return a.client.Chat(ctx, llmclient.ChatRequest{
			System:    "You enrich a durable LLM Wiki graph. Exact Go/Wiki edges are authoritative. Infer only useful document-semantic relations supported by the supplied node metadata and exact edges. Never invent node ids.",
			User:      "Return JSON only as {\"relations\":[{\"source\":\"existing id\",\"target\":\"existing id\",\"relation\":\"DOCUMENTS|REFERENCES|CITES|SUPPORTS|CONTRADICTS|RELATED_TO\",\"confidence_score\":0.55,\"rationale\":\"short evidence rationale\"}]}. INFERRED scores must be 0.55-0.95; uncertain AMBIGUOUS scores must be 0.10-0.30. Input:\n" + string(data),
			MaxTokens: a.maxOutputTokens, Temperature: 0.1, DisableThinking: a.disableThinking,
		})
	})
	if err != nil {
		return nil, err
	}
	content = extractJSONObject(content)
	var response struct {
		Relations []GraphRelationSuggestion `json:"relations"`
	}
	if err := json.Unmarshal([]byte(content), &response); err != nil {
		return nil, fmt.Errorf("decode graph semantic relations: %w", err)
	}
	return response.Relations, nil
}

func graphRelationInput(graph WikiGraphAPIResult, nodeLimit, edgeLimit int) GraphRelationInput {
	nodes := append([]WikiGraphAPINode(nil), graph.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.InDegree+a.OutDegree != b.InDegree+b.OutDegree {
			return a.InDegree+a.OutDegree > b.InDegree+b.OutDegree
		}
		return a.ID < b.ID
	})
	if len(nodes) > nodeLimit {
		nodes = nodes[:nodeLimit]
	}
	allowedNodes := map[string]bool{}
	input := GraphRelationInput{}
	for _, node := range nodes {
		if node.Kind == string(codegraph.NodeCommunity) || node.Kind == string(codegraph.NodeProcess) || node.Kind == string(codegraph.NodeFolder) {
			continue
		}
		allowedNodes[node.ID] = true
		input.Nodes = append(input.Nodes, GraphRelationNode{ID: node.ID, Domain: node.Domain, Kind: node.Kind, Label: node.Label, SourceRef: firstNonEmptyString(node.SourceRef, node.Path)})
	}
	for _, edge := range graph.Edges {
		if len(input.Edges) >= edgeLimit {
			break
		}
		if allowedNodes[edge.Source] && allowedNodes[edge.Target] && edge.Confidence == "EXTRACTED" {
			input.Edges = append(input.Edges, GraphRelationEdge{Source: edge.Source, Target: edge.Target, Relation: edge.Relation})
		}
	}
	return input
}

func loadRelationsArtifact(projectPath string) relationsArtifact {
	data, err := os.ReadFile(filepath.Join(projectPath, ".kbcore", "relations.json"))
	if err != nil {
		return relationsArtifact{Version: 1, Edges: []WikiGraphAPIEdge{}}
	}
	var artifact relationsArtifact
	if json.Unmarshal(data, &artifact) != nil {
		return relationsArtifact{Version: 1, Edges: []WikiGraphAPIEdge{}}
	}
	return artifact
}

func saveRelationsArtifact(projectPath string, artifact relationsArtifact) error {
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(projectPath, ".kbcore"), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(projectPath, ".kbcore", "relations.json"), data)
}

func minPositive(value, maximum int) int {
	if value <= 0 {
		return maximum
	}
	if value > maximum {
		return maximum
	}
	return value
}
